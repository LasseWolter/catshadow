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

// Creates a new catshadow client and returns the client
func createClient(cfg *config.Config, stateFile string) *catshadow.Client {

	// Decrypt and load the state file.
	fmt.Print("Taking hardcoded statefile decryption passphrase")
	sendPassphrase := []byte("test") // hardcode passphrase to test for now
	fmt.Print("\n")

	var stateWorker *catshadow.StateWriter
	var state *catshadow.State
	var cli *catshadow.Client
	sendC, err := client.New(cfg)
	if err != nil {
		panic(err)
	}
	// Check if statefile already exists, if not create one
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		stateWorker, state, err = catshadow.LoadStateWriter(sendC.GetLogger("catshadow_state"), stateFile, sendPassphrase)
		if err != nil {
			panic(err)
		}
		cli, err = catshadow.New(sendC.GetBackendLog(), sendC, stateWorker, state)
		if err != nil {
			panic(err)
		}
	} else { // Statefile doesn't yet exists - create one
		linkKey, err := ecdh.NewKeypair(rand.Reader)
		if err != nil {
			panic(err)
		}
		fmt.Println("registering cli with mixnet Provider")
		user := randUser()
		err = client.RegisterClient(cfg, user, linkKey.PublicKey())
		if err != nil {
			panic(err)
		}
		stateWorker, err = catshadow.NewStateWriter(sendC.GetLogger("catshadow_state"), stateFile, sendPassphrase)
		if err != nil {
			panic(err)
		}
		fmt.Println("creating remote message receiver spool")
		cli, err = catshadow.NewClientAndRemoteSpool(sendC.GetBackendLog(), sendC, stateWorker, user, linkKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("catshadow cli successfully created")
	}
	stateWorker.Start()
	fmt.Println("state worker started")
	cli.Start()
	fmt.Println("catshadow worker started")

	return cli
}

func main() {
	// Config flags
	sendCfgFile := "alice.toml"
	sendStateFile := "sender"

	// Load Sender config file.
	cfg, err := config.LoadFile(sendCfgFile)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to load config file '%v': %v\n", sendCfgFile, err)
		os.Exit(-1)
	}

	// Create client(s)
	sender := createClient(cfg, sendStateFile)

	// Start Experiment
	expDuration := time.Duration(cfg.Experiment.Duration) * time.Minute
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
