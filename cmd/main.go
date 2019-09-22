// main.go - main function of client
// Copyright (C) 2019  David Stainton.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"flag"
	"fmt"
	"github.com/katzenpost/catshadow"
	"github.com/katzenpost/client"
	"github.com/katzenpost/client/config"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/rand"
	"golang.org/x/crypto/ssh/terminal"
	"math"
	"os"
	"syscall"
	"time"
)

func randUser() string {
	user := [32]byte{}
	_, err := rand.Reader.Read(user[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", user[:])
}

func main() {
	const defaultMsgNum = 1
	const defaultMsgInterval = 2000
	const defaultBlockSize = 1

	generate := flag.Bool("g", false, "Generate the state file and then run client.")
	cfgFile := flag.String("f", "katzenpost.toml", "Path to the client config file.")
	stateFile := flag.String("s", "catshadow_statefile", "The catshadow state file path.")
	spawnShell := flag.Bool("shell", false, "Spawns a shell to interact with the catshadow client")
	message := flag.String("m", "", "Text you want to send as message")
	nickName := flag.String("n", "", "Nickname of recipient you want to send a message to")
	messageNum := flag.Int("num", defaultMsgNum, "Total number of messages you want to send")
	interval := flag.Int("i", defaultMsgInterval, "Interval between two blocks of messages being sent [in ms]")
	blockSize := flag.Int("b", defaultBlockSize, "Number of messages sent at a time")
	flag.Parse()

	//Check for invalid input and possibly return
	if (*message != "") != (*nickName != "") {
		fmt.Println("You set one of message (-m) and nickName (-n) flags without the other, that doesn't work")
		return
	}
	if (*message == "") || (*nickName == "") {
		if *messageNum != defaultMsgNum || *interval != defaultMsgInterval || *blockSize != defaultBlockSize {
			fmt.Println("To set flags -num, -i and -b you need to specify both message (-m) and nickname (-n)")
			return
		}
	}

	// Set the umask to something "paranoid".
	syscall.Umask(0077)

	fmt.Println("Katzenpost is still pre-alpha.  DO NOT DEPEND ON IT FOR STRONG SECURITY OR ANONYMITY.")

	// Load config file.
	cfg, err := config.LoadFile(*cfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config file '%v': %v\n", *cfgFile, err)
		os.Exit(-1)
	}

	// Decrypt and load the state file.
	fmt.Print("Enter statefile decryption passphrase: ")
	passphrase, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		panic(err)
	}
	fmt.Print("\n")

	var stateWorker *catshadow.StateWriter
	var state *catshadow.State
	var catShadowClient *catshadow.Client
	c, err := client.New(cfg)
	if err != nil {
		panic(err)
	}
	if *generate {
		if _, err := os.Stat(*stateFile); !os.IsNotExist(err) {
			panic("cannot generate state file, already exists")
		}
		linkKey, err := ecdh.NewKeypair(rand.Reader)
		if err != nil {
			panic(err)
		}
		fmt.Println("registering client with mixnet Provider")
		user := randUser()
		err = client.RegisterClient(cfg, user, linkKey.PublicKey())
		if err != nil {
			panic(err)
		}
		stateWorker, err = catshadow.NewStateWriter(c.GetLogger("catshadow_state"), *stateFile, passphrase)
		if err != nil {
			panic(err)
		}
		fmt.Println("creating remote message receiver spool")
		catShadowClient, err = catshadow.NewClientAndRemoteSpool(c.GetBackendLog(), c, stateWorker, user, linkKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("catshadow client successfully created")
	} else {
		stateWorker, state, err = catshadow.LoadStateWriter(c.GetLogger("catshadow_state"), *stateFile, passphrase)
		if err != nil {
			panic(err)
		}
		catShadowClient, err = catshadow.New(c.GetBackendLog(), c, stateWorker, state)
		if err != nil {
			panic(err)
		}
	}
	stateWorker.Start()
	fmt.Println("state worker started")
	catShadowClient.Start()
	fmt.Println("catshadow worker started")
	if *message != "" && *nickName != "" {
		fmt.Printf("About to send %v messages in blocks of %v - time between message blocks: %vms\n", *messageNum, *blockSize, *interval)
		if *messageNum != defaultMsgNum {
			blockNum := math.Floor(float64(*messageNum) / float64(*blockSize))
			for i := 0; i < int(blockNum); i++ {
				time.Sleep(time.Duration(*interval) * time.Millisecond)
				for b := 0; b < *blockSize; b++ {
					catShadowClient.SendMessage(*nickName, []byte(*message))
				}
			}
		}
		catShadowClient.Shutdown() // ensures that client shuts down properly - waits for pending messages to be sent
		fmt.Println("Finished sending all messages.")
	}
	if *spawnShell {
		fmt.Println("starting shell")
		shell := NewShell(catShadowClient, c.GetLogger("catshadow_shell"))
		shell.Run()
	}
}
