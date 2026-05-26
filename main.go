package main

import (
	"flag"
	"log"

	"github.com/shivam30303/diceDB/config"
	"github.com/shivam30303/diceDB/server"
)

func setupFlags() {
	flag.StringVar(&config.Host, "host", "0.0.0.0", "host for the dice server")
	flag.IntVar(&config.Port, "port", 7379, "port of the dice server")
	flag.Parse()
}

func main() {
	setupFlags()
	log.Println("rolling the dice 🎲")
	err := server.RunAsyncTCPServer()
	log.Printf("error : ", err)
}
