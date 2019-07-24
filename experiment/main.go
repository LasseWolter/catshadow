package main

import (
	"fmt"
	"github.com/common-nighthawk/go-figure"
	"github.com/katzenpost/catshadow"
	"github.com/katzenpost/client"
	"github.com/katzenpost/client/config"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/rand"
	"os"
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
	// Config flags
	sendCfgFile := "alice.toml"
	sendStateFile := "sender"

	//----------------------------------
	// SENDER SETUP
	//----------------------------------
	// Load Sender config file.
	sendCfg, err := config.LoadFile(sendCfgFile)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to load config file '%v': %v\n", sendCfgFile, err)
		os.Exit(-1)
	}

	// Decrypt and load the state file.
	fmt.Print("Taking hardcoded statefile decryption passphrase")
	sendPassphrase := []byte("test") // hardcode passphrase to test for now
	fmt.Print("\n")

	var sendStateWorker *catshadow.StateWriter
	var sendState *catshadow.State
	var sender *catshadow.Client
	sendC, err := client.New(sendCfg)
	if err != nil {
		panic(err)
	}
	// Check if statefile already exists, if not create one
	if _, err := os.Stat(sendStateFile); !os.IsNotExist(err) {
		sendStateWorker, sendState, err = catshadow.LoadStateWriter(sendC.GetLogger("catshadow_sendState"), sendStateFile, sendPassphrase)
		if err != nil {
			panic(err)
		}
		sender, err = catshadow.New(sendC.GetBackendLog(), sendC, sendStateWorker, sendState)
		if err != nil {
			panic(err)
		}
	} else { // Statefile doesn't yet exists - create one
		linkKey, err := ecdh.NewKeypair(rand.Reader)
		if err != nil {
			panic(err)
		}
		fmt.Println("registering client with mixnet Provider")
		user := randUser()
		err = client.RegisterClient(sendCfg, user, linkKey.PublicKey())
		if err != nil {
			panic(err)
		}
		sendStateWorker, err = catshadow.NewStateWriter(sendC.GetLogger("catshadow_sendState"), sendStateFile, sendPassphrase)
		if err != nil {
			panic(err)
		}
		fmt.Println("creating remote message receiver spool")
		sender, err = catshadow.NewClientAndRemoteSpool(sendC.GetBackendLog(), sendC, sendStateWorker, user, linkKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("catshadow client successfully created")
	}
	sendStateWorker.Start()
	fmt.Println("state worker started")
	sender.Start()
	fmt.Println("catshadow worker started")
	expDuration := time.Duration(sendCfg.Experiment.Duration) * time.Minute
	startTime := time.Now()
	// Display Header of Experiment
	printFigure("Mixnet", "epic")
	printFigure("Experiment", "epic")

	// Wait for expDuration - the sending happens automatically when lambdaP triggers
	fmt.Printf("\nThe experiment will run for %v\nIt'll finish at: %v\n", expDuration, startTime.Add(expDuration))
	fmt.Println("Messages are sent according to a Poisson Process")
	// Update output on regular intervals to display how long the experiment will last for
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			fmt.Printf("The experiment will finish in %v\n", time.Until(startTime.Add(expDuration)).Truncate(1*time.Second))
		}
	}()
	time.Sleep(expDuration)

	// Experiment finished, stop everything
	ticker.Stop()
	sender.Shutdown()
}

// Prints string in given font as Ascii-Art
func printFigure(str string, font string) {
	fig := figure.NewFigure(str, font, true)
	fig.Print()
}
