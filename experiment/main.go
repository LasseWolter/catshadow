package main

import (
	"fmt"
	"github.com/katzenpost/catshadow"
	"github.com/katzenpost/client"
	"github.com/katzenpost/client/config"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/rand"
	"os"
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
	recCfgFile := "alice.toml"
	recStateFile := "receiver"
	sendCfgFile := "alice.toml"
	sendStateFile := "sender"

	//----------------------------------
	// RECEIVER SETUP
	//----------------------------------
	// Load Receiver config file.
	recCfg, err := config.LoadFile(recCfgFile)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to load config file '%v': %v\n", recCfgFile, err)
		os.Exit(-1)
	}

	// Decrypt and load the state file.
	fmt.Print("Enter statefile decryption passphrase: ")
	recPassphrase := []byte("test") // hardcode passphrase to test for now
	fmt.Print("\n")

	var recStateWorker *catshadow.StateWriter
	var recState *catshadow.State
	var recCatShadowClient *catshadow.Client
	recC, err := client.New(recCfg)
	if err != nil {
		panic(err)
	}
	// Check if statefile already exists, if not create one
	if _, err := os.Stat(recStateFile); !os.IsNotExist(err) {
		recStateWorker, recState, err = catshadow.LoadStateWriter(recC.GetLogger("catshadow_recState"), recStateFile, recPassphrase)
		if err != nil {
			panic(err)
		}
		recCatShadowClient, err = catshadow.New(recC.GetBackendLog(), recC, recStateWorker, recState)
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
		err = client.RegisterClient(recCfg, user, linkKey.PublicKey())
		if err != nil {
			panic(err)
		}
		recStateWorker, err = catshadow.NewStateWriter(recC.GetLogger("catshadow_recState"), recStateFile, recPassphrase)
		if err != nil {
			panic(err)
		}
		fmt.Println("creating remote message receiver spool")
		recCatShadowClient, err = catshadow.NewClientAndRemoteSpool(recC.GetBackendLog(), recC, recStateWorker, user, linkKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("catshadow client successfully created")
	}
	recStateWorker.Start()
	fmt.Println("state worker started")
	recCatShadowClient.Start()
	fmt.Println("catshadow worker started")

	//----------------------------------
	// SENDER SETUP
	//----------------------------------
	// Load Sender config file.
	sendCfg, err := config.LoadFile(sendCfgFile)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to load config file '%v': %v\n", recCfgFile, err)
		os.Exit(-1)
	}

	// Decrypt and load the state file.
	fmt.Print("Enter statefile decryption passphrase: ")
	sendPassphrase := []byte("test") // hardcode passphrase to test for now
	fmt.Print("\n")

	var sendStateWorker *catshadow.StateWriter
	var sendState *catshadow.State
	var sendCatShadowClient *catshadow.Client
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
		sendCatShadowClient, err = catshadow.New(sendC.GetBackendLog(), sendC, sendStateWorker, sendState)
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
		sendCatShadowClient, err = catshadow.NewClientAndRemoteSpool(sendC.GetBackendLog(), sendC, sendStateWorker, user, linkKey)
		if err != nil {
			panic(err)
		}
		fmt.Println("catshadow client successfully created")
	}
	sendStateWorker.Start()
	fmt.Println("state worker started")
	sendCatShadowClient.Start()
	fmt.Println("catshadow worker started")

}
