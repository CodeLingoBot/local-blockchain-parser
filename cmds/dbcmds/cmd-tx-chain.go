package dbcmds

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"

	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/blockdb"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/cmds/utils"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/cmds/utils/aeskeyfind"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/scanner"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/scanner/detector"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/scanner/detectoroutput"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/scanner/txdatasource"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/scanner/txdatasourceoutput"
	"github.com/WikiLeaksFreedomForce/local-blockchain-parser/scanner/txhashsource"
)

type TxChainCommand struct {
	dbFile     string
	datFileDir string
	txHash     string
	outDir     string
	db         *blockdb.BlockDB
}

func NewTxChainCommand(datFileDir, dbFile, outDir, txHash string) *TxChainCommand {
	return &TxChainCommand{
		dbFile:     dbFile,
		datFileDir: datFileDir,
		txHash:     txHash,
		outDir:     filepath.Join(outDir, "tx-chain", txHash),
	}
}

func (cmd *TxChainCommand) RunCommand() error {
	err := os.MkdirAll(cmd.outDir, 0777)
	if err != nil {
		return err
	}

	db, err := blockdb.NewBlockDB(cmd.dbFile, cmd.datFileDir)
	if err != nil {
		return err
	}
	defer db.Close()

	cmd.db = db

	startHash, err := blockdb.HashFromString(cmd.txHash)
	if err != nil {
		return err
	}

	s := &scanner.Scanner{
		DB:           db,
		TxHashSource: txhashsource.NewChain(db, startHash),
		TxDataSources: []scanner.ITxDataSource{
			&txdatasource.InputScripts{},
			&txdatasource.OutputScripts{},
			&txdatasource.OutputScriptsSatoshi{},
		},
		TxDataSourceOutputs: []scanner.ITxDataSourceOutput{
			&txdatasourceoutput.RawData{OutDir: cmd.outDir},
		},
		Detectors: []scanner.IDetector{
			&detector.PGPPackets{},
			&detector.AESKeys{},
			&detector.MagicBytes{},
			// &detector.Plaintext{},
		},
		DetectorOutputs: []scanner.IDetectorOutput{
			&detectoroutput.Console{Prefix: "  - "},
			&detectoroutput.RawData{OutDir: cmd.outDir},
			&detectoroutput.CSV{OutDir: cmd.outDir},
			&detectoroutput.CSVTxAnalysis{OutDir: cmd.outDir},
		},
	}

	err = s.Run()
	if err != nil {
		return err
	}

	return s.Close()

	// foundHashes, err := cmd.getTxs(startHash)
	// if err != nil {
	// 	return err
	// }

	// err = cmd.processTxs(startHash, foundHashes)
	// if err != nil {
	// 	return err
	// }

	// transactionTxt := ""
	// for _, h := range foundHashes {
	// 	transactionTxt = transactionTxt + h.String() + "\n"
	// }
	// err = utils.CreateAndWriteFile(filepath.Join(cmd.outDir, "transactions.txt"), []byte(transactionTxt))
	// if err != nil {
	// 	return err
	// }

	// return nil
}

func (cmd *TxChainCommand) getTxs(startHash chainhash.Hash) ([]chainhash.Hash, error) {
	foundHashes1, _ := cmd.crawlBackwards(startHash)
	// if err != nil {
	// 	return nil, err
	// }

	foundHashes2, _ := cmd.crawlForwards(startHash)
	// if err != nil {
	// 	return nil, err
	// }

	// both foundHashes1 and foundHashes2 contain startHash, so we omit it from one of them
	foundHashes := append(foundHashes1, foundHashes2[1:]...)

	return foundHashes, nil
}

func (cmd *TxChainCommand) crawlBackwards(startHash chainhash.Hash) ([]chainhash.Hash, error) {
	foundHashesReverse := []chainhash.Hash{}
	currentTxHash := startHash
	for {
		tx, err := cmd.db.GetTx(currentTxHash)
		if err != nil {
			return nil, err
		}

		if utils.TxHasSuspiciousOutputValues(tx) {
			foundHashesReverse = append(foundHashesReverse, currentTxHash)
			if len(tx.MsgTx().TxIn) == 1 {
				currentTxHash = tx.MsgTx().TxIn[0].PreviousOutPoint.Hash
			} else {
				break
			}
		} else {
			break
		}
	}

	numHashes := len(foundHashesReverse)
	foundHashes := make([]chainhash.Hash, numHashes)
	for i := 0; i < numHashes; i++ {
		foundHashes[numHashes-i-1] = foundHashesReverse[i]
	}

	return foundHashes, nil
}

func (cmd *TxChainCommand) crawlForwards(startHash chainhash.Hash) ([]chainhash.Hash, error) {
	foundHashes := []chainhash.Hash{}
	currentTxHash := startHash
	for {
		tx, err := cmd.db.GetTx(currentTxHash)
		if err != nil {
			return foundHashes, err
		}

		foundHashes = append(foundHashes, currentTxHash)

		maxValueTxoutIdx := utils.FindMaxValueTxOut(tx)

		key := blockdb.SpentTxOutKey{TxHash: *tx.Hash(), TxOutIndex: uint32(maxValueTxoutIdx)}
		spentTxOut, err := cmd.db.GetSpentTxOut(key)
		if err != nil {
			// return nil, err
			// fmt.Printf("Can't find where TxOut %+v was spent.  Either it was unspent, or you haven't indexed the .dat files containing it.\n", key)

			fmt.Printf("searching for TxOut %+v in DAT files\n", key)
			spentTxOut, err = cmd.db.GetSpentTxOutFromDATFiles(key)
			if err != nil {
				return foundHashes, err
			}

			// break
		}

		currentTxHash = spentTxOut.InputTxHash
	}
	return foundHashes, nil
}

func (cmd *TxChainCommand) processTxs(startHash chainhash.Hash, txHashes []chainhash.Hash) error {
	err := cmd.writeSatoshiDataFromTxOuts(txHashes)
	if err != nil {
		return err
	}

	err = cmd.writeDataFromTxOuts(txHashes)
	if err != nil {
		return err
	}

	err = cmd.checkFileMagicBytes(txHashes)
	if err != nil {
		return err
	}

	err = cmd.checkPlaintext(txHashes)
	if err != nil {
		return err
	}

	err = cmd.checkPGPPackets(txHashes)
	if err != nil {
		return err
	}

	err = cmd.checkAESKeys(txHashes)
	if err != nil {
		return err
	}

	return nil
}

func (cmd *TxChainCommand) checkAESKeys(txHashes []chainhash.Hash) error {
	csvFilename := filepath.Join(cmd.outDir, "aes-keys.csv")
	csvFile := utils.NewConditionalFile(csvFilename)
	defer csvFile.Close()

	_, err := csvFile.WriteString("tx hash,in/out,description\n", false)
	if err != nil {
		return err
	}

	type txDataSource struct {
		name    string
		getData func(*btcutil.Tx) ([]byte, error)
	}

	txDataSources := []txDataSource{
		{"input", utils.ConcatTxInScripts},
		{"output", utils.ConcatNonOPHexTokensFromTxOuts},
		{"output-satoshi", utils.ConcatSatoshiDataFromTxOuts},
	}

	type IResult interface {
		DescriptionStrings() []string
		IsEmpty() bool
	}

	outputMethods := []func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error{
		func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error {
			for _, p := range result.DescriptionStrings() {
				fmt.Printf("  - %v AES key detected: %s\n", txDataSourceName, p)
			}
			return nil
		},
		func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error {
			for _, p := range result.DescriptionStrings() {
				_, err := csvFile.WriteString(fmt.Sprintf("%s,%s,%s\n", txHash.String(), txDataSourceName, p), true)
				if err != nil {
					return err
				}
			}
			return nil
		},
		func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error {
			if !result.IsEmpty() {
				return utils.CreateAndWriteFile(filepath.Join(cmd.outDir, fmt.Sprintf("aes-data-%s-%s.dat", txHash.String(), txDataSourceName)), data)
			}
			return nil
		},
	}

	for _, txHash := range txHashes {
		tx, err := cmd.db.GetTx(txHash)
		if err != nil {
			return err
		}

		for _, txDataSource := range txDataSources {
			data, err := txDataSource.getData(tx)
			if err != nil {
				continue
			}

			result := aeskeyfind.Detect(data)

			for _, out := range outputMethods {
				err := out(txHash, txDataSource.name, data, result)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (cmd *TxChainCommand) checkPGPPackets(txHashes []chainhash.Hash) error {
	csvFilename := filepath.Join(cmd.outDir, "pgp-packets.csv")
	csvFile := utils.NewConditionalFile(csvFilename)
	defer csvFile.Close()

	_, err := csvFile.WriteString("tx hash,in/out,description\n", false)
	if err != nil {
		return err
	}

	type txDataSource struct {
		name    string
		getData func(*btcutil.Tx) ([]byte, error)
	}

	txDataSources := []txDataSource{
		{"input", utils.ConcatTxInScripts},
		{"output", utils.ConcatNonOPHexTokensFromTxOuts},
		{"output-satoshi", utils.ConcatSatoshiDataFromTxOuts},
	}

	type IResult interface {
		DescriptionStrings() []string
		IsEmpty() bool
	}

	outputMethods := []func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error{
		// func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error {
		// 	for _, p := range result.DescriptionStrings() {
		// 		fmt.Printf("  - %v PGP packet detected: %s\n", txDataSourceName, p)
		// 	}
		// 	return nil
		// },
		func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error {
			for _, p := range result.DescriptionStrings() {
				_, err := csvFile.WriteString(fmt.Sprintf("%s,%s,%s\n", txHash.String(), txDataSourceName, p), true)
				if err != nil {
					return err
				}
			}
			return nil
		},
		func(txHash chainhash.Hash, txDataSourceName string, data []byte, result IResult) error {
			if !result.IsEmpty() {
				return utils.CreateAndWriteFile(filepath.Join(cmd.outDir, fmt.Sprintf("pgp-data-%s-%s.dat", txHash.String(), txDataSourceName)), data)
			}
			return nil
		},
	}

	for _, txHash := range txHashes {
		tx, err := cmd.db.GetTx(txHash)
		if err != nil {
			return err
		}

		for _, txDataSource := range txDataSources {
			data, err := txDataSource.getData(tx)
			if err != nil {
				continue
			}

			result := utils.FindPGPPackets(data)

			for _, out := range outputMethods {
				err := out(txHash, txDataSource.name, data, result)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (cmd *TxChainCommand) checkFileMagicBytes(txHashes []chainhash.Hash) error {
	outFilename := filepath.Join(cmd.outDir, "file-magic.csv")
	outFile := utils.NewConditionalFile(outFilename)
	defer outFile.Close()

	_, err := outFile.WriteString("tx hash,in/out,description\n", false)
	if err != nil {
		return err
	}

	for _, txHash := range txHashes {
		tx, err := cmd.db.GetTx(txHash)
		if err != nil {
			return err
		}

		inData, err := utils.ConcatTxInScripts(tx)
		if err != nil {
			return err
		}

		matches := utils.SearchDataForMagicFileBytes(inData)
		for _, m := range matches {
			fmt.Printf("  - input scripts file detected: %s\n", m.Description())
			_, err := outFile.WriteString(fmt.Sprintf("%s,input,%s\n", txHash.String(), m.Description()), true)
			if err != nil {
				return err
			}
		}

		outData, err := utils.ConcatNonOPHexTokensFromTxOuts(tx)
		if err != nil {
			return err
		}

		matches = utils.SearchDataForMagicFileBytes(outData)
		for _, m := range matches {
			fmt.Printf("  - output scripts file detected: %s\n", m.Description())
			_, err := outFile.WriteString(fmt.Sprintf("%s,output,%s\n", txHash.String(), m.Description()), true)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (cmd *TxChainCommand) checkPlaintext(txHashes []chainhash.Hash) error {
	outFilename := filepath.Join(cmd.outDir, "plaintext.csv")
	outFile := utils.NewConditionalFile(outFilename)
	defer outFile.Close()

	_, err := outFile.WriteString("tx hash,in/out,text\n", false)
	if err != nil {
		return err
	}

	for _, txHash := range txHashes {
		tx, err := cmd.db.GetTx(txHash)
		if err != nil {
			return err
		}

		inData, err := utils.ConcatTxInScripts(tx)
		if err != nil {
			return err
		}

		textBytes := utils.StripNonTextBytes(inData)
		if len(textBytes) > 16 {
			// fmt.Printf("  - input scripts plaintext detected: %s\n", string(textBytes))
			_, err := outFile.WriteString(fmt.Sprintf("%s,input,%s\n", txHash.String(), string(textBytes)), true)
			if err != nil {
				return err
			}
		}

		outData, err := utils.ConcatNonOPHexTokensFromTxOuts(tx)
		if err != nil {
			return err
		}

		textBytes = utils.StripNonTextBytes(outData)
		if len(textBytes) > 16 {
			// fmt.Printf("  - output scripts plaintext detected: %s\n", string(textBytes))
			_, err := outFile.WriteString(fmt.Sprintf("%s,output,%s\n", txHash.String(), string(textBytes)), true)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (cmd *TxChainCommand) writeSatoshiDataFromTxOuts(txHashes []chainhash.Hash) error {
	outFilename := filepath.Join(cmd.outDir, "satoshi-encoded-data.dat")
	outFile := utils.NewConditionalFile(outFilename)
	defer outFile.Close()

	for _, txHash := range txHashes {
		tx, err := cmd.db.GetTx(txHash)
		if err != nil {
			return err
		}

		data := []byte{}
		// we skip the final two TxOuts because one goes to WL and one is used to pass BTC to the next transaction in the chain
		for i := 0; i < len(tx.MsgTx().TxOut)-2; i++ {
			bs, err := utils.GetNonOPBytes(tx.MsgTx().TxOut[i].PkScript)
			if err != nil {
				continue
			}

			data = append(data, bs...)
		}

		data, err = utils.GetSatoshiEncodedData(data)
		if err != nil {
			return nil
			// return err
		}

		_, err = outFile.Write(data, true)
		if err != nil {
			return err
		}
	}

	fmt.Println("Satoshi-encoded data written to", outFilename)
	return nil
}

func (cmd *TxChainCommand) writeDataFromTxOuts(txHashes []chainhash.Hash) error {
	outFilename := filepath.Join(cmd.outDir, "txout-data.dat")
	outFile := utils.NewConditionalFile(outFilename)
	defer outFile.Close()

	for _, txHash := range txHashes {
		tx, err := cmd.db.GetTx(txHash)
		if err != nil {
			return err
		}

		data := []byte{}
		for i := 0; i < len(tx.MsgTx().TxOut); i++ {
			bs, err := utils.GetNonOPBytes(tx.MsgTx().TxOut[i].PkScript)
			if err != nil {
				continue
			}

			data = append(data, bs...)
		}

		_, err = outFile.Write(data, true)
		if err != nil {
			return err
		}
	}

	fmt.Println("TxOut data written to", outFilename)
	return nil
}
