package main

import (
	"fmt"
	"github.com/common-nighthawk/go-figure"
	"github.com/katzenpost/catshadow"
	"github.com/katzenpost/client"
	"github.com/katzenpost/client/config"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/rand"
	"math"
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
	messageNum := sendCfg.Experiment.MessageNum
	blockSize := sendCfg.Experiment.BlockSize
	interval := sendCfg.Experiment.Interval
	expDuration := time.Duration(sendCfg.Experiment.Duration) * time.Minute
	// Display Header of Experiment
	printFigure("Mixnet", "epic")
	printFigure("Experiment", "epic")
	// If an experiment duration is given use this instead of sending `messageNum` messages
	if expDuration > time.Duration(0) {
		printFigure("by Duration", "small")
		fmt.Printf("\nFound Duration in Config file. Ignoring MessageNum and will keep sending messages until "+
			"experiment is over. Experiment will last: %v\n\n", expDuration)
		running := true
		// Boolean `running` will be set to false after expDuration and will cause the for loop below to stop
		time.AfterFunc(expDuration, func() {
			running = false
		})
		// Worker loop which keeps sending messages until the end of the experiment
		for running == true {
			for b := 0; b < blockSize; b++ {
				sender.DoSendDropMsg()
			}
			fmt.Printf("Sent message block of %v message(s)\n", blockSize)
			time.Sleep(time.Duration(interval) * time.Millisecond)
		}
	} else { // No experiment duratoin was given. The experiment will last unitl messageNum messages have been sent
		printFigure("by Message Number", "small")
		fmt.Printf("\nAbout to send %v messages in blocks of %v - time between message blocks: %vms\n", messageNum, blockSize, interval)
		blockNum := math.Floor(float64(messageNum) / float64(blockSize))
		for i := 0; i < int(blockNum); i++ {
			for b := 0; b < blockSize; b++ {
				sender.DoSendDropMsg()
			}
			fmt.Printf("Sent message block of %v message(s)\n", blockSize)
			time.Sleep(time.Duration(interval) * time.Millisecond)
		}
	}
	sender.Shutdown()
}

func printFigure(str string, font string) {
	fig := figure.NewFigure(str, font, true)
	fig.Print()
}
