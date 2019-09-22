package main

import (
	"flag"
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

// Allows client to try creation several times before it throws a panic
func retryConnect(err error, cfg *config.Config, stateFile string, try int) *catshadow.Client {
	// Connection to provider failed 5 times
	if try >= 5 {
		panic(err)
	}
	// Retry connecting to provider
	fmt.Println(err, " Retry client creation...")
	_ = os.Remove(stateFile)
	try++
	return createClient(cfg, stateFile, try)

}

// Creates a new catshadow client and returns the client
func createClient(cfg *config.Config, stateFile string, try int) *catshadow.Client {

	// Remove existing statefile to guarantee clean environment
	if _, err := os.Stat(stateFile); err == nil {
		fmt.Printf("Removed existing statefile \"%v\" to start in a clean environment\n", stateFile)
		_ = os.Remove(stateFile)
	}
	// Decrypt and load the state file.
	fmt.Print("Taking hardcoded statefile decryption passphrase")
	sendPassphrase := []byte("test") // hardcode passphrase to test for now
	fmt.Print("\n")

	var stateWorker *catshadow.StateWriter
	var state *catshadow.State
	var cli *catshadow.Client
	sendC, err := client.New(cfg)

	if err != nil {
		return retryConnect(err, cfg, stateFile, try)
	}
	// Check if statefile already exists, if not create one
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		stateWorker, state, err = catshadow.LoadStateWriter(sendC.GetLogger("catshadow_state"), stateFile, sendPassphrase)
		if err != nil {
			return retryConnect(err, cfg, stateFile, try)
		}
		cli, err = catshadow.New(sendC.GetBackendLog(), sendC, stateWorker, state)
		if err != nil {
			return retryConnect(err, cfg, stateFile, try)
		}
	} else { // Statefile doesn't yet exists - create one
		linkKey, err := ecdh.NewKeypair(rand.Reader)
		if err != nil {
			return retryConnect(err, cfg, stateFile, try)
		}
		fmt.Println("registering cli with mixnet Provider")
		user := randUser()
		err = client.RegisterClient(cfg, user, linkKey.PublicKey())
		if err != nil {
			return retryConnect(err, cfg, stateFile, try)
		}
		stateWorker, err = catshadow.NewStateWriter(sendC.GetLogger("catshadow_state"), stateFile, sendPassphrase)
		if err != nil {
			return retryConnect(err, cfg, stateFile, try)
		}
		fmt.Println("creating remote message receiver spool")
		cli, err = catshadow.NewClientAndRemoteSpool(sendC.GetBackendLog(), sendC, stateWorker, user, linkKey)
		if err != nil {
			return retryConnect(err, cfg, stateFile, try)
		}
		fmt.Println("catshadow cli successfully created")
	}
	stateWorker.Start()
	fmt.Println("state worker started for: ", stateFile)
	cli.Start()
	fmt.Println("catshadow worker started for: ", stateFile)

	return cli
}

// Allows logging a message for all clients as well as printing it to STDOUT
func cliLog(clients map[string]*catshadow.Client, logMsg string) {
	for name, cli := range clients {
		cli.GetLogger().Infof("[EXPERIMENT][%v] %v", name, logMsg)
	}
	fmt.Println(logMsg)
}

func main() {
	cfgFile := flag.String("f", "alice.toml", "Path to the client config file")
	flag.Parse()

	// Load Sender config file.
	cfg, err := config.LoadFile(*cfgFile)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to load config file '%v': %v\n", *cfgFile, err)
		os.Exit(-1)
	}

	// Create client(s)
	//c1 := createClient(cfg, "c1")
	//c2 := createClient(cfg, "c2")
	clients := make(map[string]*catshadow.Client)

	for _, c := range cfg.Client {
		cli := createClient(cfg, c.Name, 1)
		clients[c.Name] = cli
		cli.GetLogger().Infof("[EXPERIMENT][%v] Client successfully created", c.Name)
	}

	// Start Experiment
	cliLog(clients, "All clients created, starting experiment.")
	expDuration := time.Duration(cfg.Experiment.Duration) * time.Minute
	startTime := time.Now()
	// Display Header of Experiment
	printFigure("Mixnet", "epic")
	printFigure("Experiment", "epic")

	// Wait for expDuration - the sending happens automatically when lambdaP triggers
	logStr := fmt.Sprintf("\nThe experiment will run for %v\nIt'll finish at: %v\n", expDuration, startTime.Add(expDuration))
	cliLog(clients, logStr)
	fmt.Println("Messages are sent according to a Poisson Process")
	// Update output on regular intervals to display how long the experiment will last for
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for range ticker.C {
			logStr := fmt.Sprintf("The experiment will finish in %v\n", time.Until(startTime.Add(expDuration)).Truncate(1*time.Second))
			cliLog(clients, logStr)
		}
	}()

	// Add async tasks for LambdaP updates according to the config file
	for _, c := range cfg.Client {
		for _, update := range c.Update {
			go func() {
				delay := time.Duration(update.Time) * time.Minute
				<-time.After(time.Until(startTime.Add(delay)))
				clients[c.Name].SetLambdaP(update.LambdaP, update.LambdaPMaxDelay)
				logStr := fmt.Sprintf("%v: SendRate Update.\n   -LambdaP: %v\n   -LambdaPMaxDelay: %v\n", c.Name, update.LambdaP, update.LambdaPMaxDelay)
				cliLog(clients, logStr)
			}()
		}
	}

	// Select blocks execution until one of it's cases is run - here there is only one case: if the experiment is over
	select {
	case <-time.After(time.Until(startTime.Add(expDuration))):
		// Experiment finished, stop everything
		cliLog(clients, "Sending finished. Stopped sending messages.")
		ticker.Stop()
		return
		// Unfortunately the shutdown command doesn't always complete which is why the return
		// without proper shutdown is the safer option to guarantee that the experiment finishes and doesn't run forever
		//for _,c := range clients {
		//c.Shutdown()
		//}
	}

}

// Prints string in given font as Ascii-Art
func printFigure(str string, font string) {
	fig := figure.NewFigure(str, font, true)
	fig.Print()
}
