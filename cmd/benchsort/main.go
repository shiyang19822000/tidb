// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
)

type comparableRow struct {
	key    []types.Datum
	val    []types.Datum
	handle int64
}

var (
	genCmd = flag.NewFlagSet("gen", flag.ExitOnError)
	runCmd = flag.NewFlagSet("gen", flag.ExitOnError)
	// helpCmd = flag.NewFlagSet("help", flag.ExitOnError)

	logLevel    = "warn"
	tmpDir      string
	keySize     int
	valSize     int
	bufSize     int
	scale       int
	inputRatio  int
	outputRatio int
)

func nextRow(r *rand.Rand, keySize int, valSize int) *comparableRow {
	key := make([]types.Datum, keySize)
	for i := range key {
		key[i] = types.NewDatum(r.Int())
	}

	val := make([]types.Datum, valSize)
	for j := range val {
		val[j] = types.NewDatum(r.Int())
	}

	handle := r.Int63()
	// cLogf("key: %d, val: %d, handle: %d", key[0].GetInt64(), val[0].GetInt64(), handle)
	return &comparableRow{key: key, val: val, handle: handle}
}

func encodeRow(b []byte, row *comparableRow) ([]byte, error) {
	var (
		err  error
		head = make([]byte, 8)
		body []byte
	)

	body, err = codec.EncodeKey(body, row.key...)
	if err != nil {
		return b, errors.Trace(err)
	}
	body, err = codec.EncodeKey(body, row.val...)
	if err != nil {
		return b, errors.Trace(err)
	}
	body, err = codec.EncodeKey(body, types.NewIntDatum(row.handle))
	if err != nil {
		return b, errors.Trace(err)
	}

	binary.BigEndian.PutUint64(head, uint64(len(body)))

	b = append(b, head...)
	b = append(b, body...)

	return b, nil
}

func decodeRow(fd *os.File) (*comparableRow, error) {
	var (
		err  error
		n    int
		head = make([]byte, 8)
		dcod = make([]types.Datum, 0, keySize+valSize+1)
	)

	n, err = fd.Read(head)
	if n != 8 {
		return nil, errors.New("incorrect header")
	}
	if err != nil {
		return nil, errors.Trace(err)
	}

	rowSize := int(binary.BigEndian.Uint64(head))
	rowBytes := make([]byte, rowSize)

	n, err = fd.Read(rowBytes)
	if n != rowSize {
		return nil, errors.New("incorrect row")
	}
	if err != nil {
		return nil, errors.Trace(err)
	}

	dcod, err = codec.Decode(rowBytes, keySize+valSize+1)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &comparableRow{
		key:    dcod[:keySize],
		val:    dcod[keySize : keySize+valSize],
		handle: dcod[keySize+valSize:][0].GetInt64(),
	}, nil
}

func encodeMeta(b []byte, scale int, keySize int, valSize int) []byte {
	meta := make([]byte, 8)

	binary.BigEndian.PutUint64(meta, uint64(scale))
	b = append(b, meta...)
	binary.BigEndian.PutUint64(meta, uint64(keySize))
	b = append(b, meta...)
	binary.BigEndian.PutUint64(meta, uint64(valSize))
	b = append(b, meta...)

	return b
}

func decodeMeta(fd *os.File) error {
	meta := make([]byte, 24)
	if n, err := fd.Read(meta); err != nil || n != 24 {
		if n != 24 {
			return errors.New("incorrect meta data")
		}
		return errors.Trace(err)
	}

	scale = int(binary.BigEndian.Uint64(meta[:8]))
	if scale <= 0 {
		return errors.New("number of rows must be positive")
	}

	keySize = int(binary.BigEndian.Uint64(meta[8:16]))
	if keySize <= 0 {
		return errors.New("key size must be positive")
	}

	valSize = int(binary.BigEndian.Uint64(meta[16:]))
	if valSize <= 0 {
		return errors.New("value size must be positive")
	}

	return nil
}

func export() error {
	var (
		err         error
		outputBytes []byte
		outputFile  *os.File
	)

	fileName := path.Join(tmpDir, "data.out")
	outputFile, err = os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return errors.Trace(err)
	}
	defer outputFile.Close()

	outputBytes = encodeMeta(outputBytes, scale, keySize, valSize)

	seed := rand.NewSource(time.Now().UnixNano())
	r := rand.New(seed)

	for i := 1; i <= scale; i++ {
		outputBytes, err = encodeRow(outputBytes, nextRow(r, keySize, valSize))
		if err != nil {
			return errors.Trace(err)
		}
	}

	_, err = outputFile.Write(outputBytes)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

func load(ratio int) ([]*comparableRow, error) {
	var (
		err error
		fd  *os.File
	)

	fileName := path.Join(tmpDir, "data.out")
	fd, err = os.Open(fileName)
	if os.IsNotExist(err) {
		return nil, errors.New("data file (data.out) does not exist")
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer fd.Close()

	err = decodeMeta(fd)
	if err != nil {
		return nil, errors.Trace(err)
	}

	cLogf("\tnumber of rows = %d, key size = %d, value size = %d", scale, keySize, valSize)

	var (
		row  *comparableRow
		data = make([]*comparableRow, 0, scale)
	)

	totalRows := int(float64(scale) * (float64(ratio) / 100.0))
	cLogf("\tload %d rows", totalRows)
	for i := 1; i <= totalRows; i++ {
		row, err = decodeRow(fd)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// cLogf("key: %d, val: %d, handle: %d",
		// row.key[0].GetInt64(), row.val[0].GetInt64(), row.handle)
		data = append(data, row)
	}

	return data, nil
}

func init() {
	log.SetLevelByString(logLevel)

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	genCmd.StringVar(&tmpDir, "dir", cwd, "where to store the generated rows")
	genCmd.IntVar(&keySize, "keySize", 8, "the size of key")
	genCmd.IntVar(&valSize, "valSize", 8, "the size of vlaue")
	genCmd.IntVar(&scale, "scale", 100, "how many rows to generate")

	runCmd.StringVar(&tmpDir, "dir", cwd, "where to load the generated rows")
	runCmd.IntVar(&bufSize, "bufSize", 500000, "how many rows held in memory at a time")
	runCmd.IntVar(&inputRatio, "inputRatio", 100, "input percentage")
	runCmd.IntVar(&outputRatio, "outputRatio", 100, "output percentage")
}

func main() {
	flag.Parse()

	if len(os.Args) == 1 {
		fmt.Println("Usage:\n")
		fmt.Println("\tbenchsort command [arguments]\n")
		fmt.Println("The commands are:\n")
		fmt.Println("\tgen\t", "generate rows")
		fmt.Println("\trun\t", "run tests")
		fmt.Println("")
		fmt.Println("Use \"benchsort help [command]\" for more information about a command.")
		return
	}

	switch os.Args[1] {
	case "gen":
		genCmd.Parse(os.Args[2:])
	case "run":
		runCmd.Parse(os.Args[2:])
	default:
		fmt.Printf("%q is not valid command.\n", os.Args[1])
		os.Exit(2)
	}

	if genCmd.Parsed() {
		// Sanity checks
		if keySize <= 0 {
			log.Fatal(errors.New("key size must be positive"))
		}
		if valSize <= 0 {
			log.Fatal(errors.New("value size must be positive"))
		}
		if scale <= 0 {
			log.Fatal(errors.New("scale must be positive"))
		}
		if _, err := os.Stat(tmpDir); err != nil {
			if os.IsNotExist(err) {
				log.Fatal(errors.New("tmpDir does not exist"))
			}
			log.Fatal(err)
		}

		cLog("Generating...")
		start := time.Now()
		if err := export(); err != nil {
			log.Fatal(err)
		}
		cLog("Done!")
		cLogf("Data placed in: %s", path.Join(tmpDir, "data.out"))
		cLog("Time used: ", time.Since(start))
	}

	if runCmd.Parsed() {
		// Sanity checks
		if bufSize <= 0 {
			log.Fatal(errors.New("buffer size must be positive"))
		}
		if inputRatio < 0 || inputRatio > 100 {
			log.Fatal(errors.New("input ratio must between 0 and 100 (inclusive)"))
		}
		if outputRatio < 0 || outputRatio > 100 {
			log.Fatal(errors.New("output ratio must between 0 and 100 (inclusive)"))
		}
		if _, err := os.Stat(tmpDir); err != nil {
			if os.IsNotExist(err) {
				log.Fatal(errors.New("tmpDir does not exist"))
			}
			log.Fatal(err)
		}

		var (
			err  error
			data []*comparableRow
		)
		cLog("Loading...")
		start := time.Now()
		data, err = load(inputRatio)
		if err != nil {
			log.Fatal(err)
		}
		cLog("Done!")
		cLog("Time used: ", time.Since(start))
		cLogf("data size: %d", len(data))
	}
}

func cLogf(format string, args ...interface{}) {
	str := fmt.Sprintf(format, args...)
	fmt.Println("\033[0;32m" + str + "\033[0m")
}

func cLog(args ...interface{}) {
	str := fmt.Sprint(args...)
	fmt.Println("\033[0;32m" + str + "\033[0m")
}
