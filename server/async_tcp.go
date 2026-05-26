package server

import (
	"log"
	"net"
	"syscall"

	"github.com/shivam30303/diceDB/config"
	"github.com/shivam30303/diceDB/core"
)

var con_clients int = 0

func RunAsyncTCPServer() error {
	log.Println("starting an asynchronous TCP server on", config.Host, config.Port)

	max_clients := 20000

	// Create kqueue event objects to hold events
	events := make([]syscall.Kevent_t, max_clients)

	// Create a socket
	serverFD, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(serverFD)

	// Set the socket to operate in non-blocking mode
	if err = syscall.SetNonblock(serverFD, true); err != nil {
		return err
	}

	// Bind the IP and the port
	ip4 := net.ParseIP(config.Host).To4()
	if err = syscall.Bind(serverFD, &syscall.SockaddrInet4{
		Port: config.Port,
		Addr: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]},
	}); err != nil {
		return err
	}

	// Start listening
	if err = syscall.Listen(serverFD, max_clients); err != nil {
		return err
	}

	// AsyncIO starts here!!

	// Create kqueue instance (replaces epoll_create1)
	kqueueFD, err := syscall.Kqueue()
	if err != nil {
		log.Fatal(err)
	}
	defer syscall.Close(kqueueFD)

	// Register the server socket for read events (replaces EpollCtl ADD)
	serverEvent := syscall.Kevent_t{
		Ident:  uint64(serverFD),
		Filter: syscall.EVFILT_READ,
		Flags:  syscall.EV_ADD,
	}
	if _, err = syscall.Kevent(kqueueFD, []syscall.Kevent_t{serverEvent}, nil, nil); err != nil {
		return err
	}

	for {
		// Wait for events (replaces EpollWait)
		nevents, e := syscall.Kevent(kqueueFD, nil, events, nil)
		if e != nil {
			continue
		}

		for i := 0; i < nevents; i++ {
			// if the socket server itself is ready for an IO
			if int(events[i].Ident) == serverFD {
				// accept the incoming connection from a client
				fd, _, err := syscall.Accept(serverFD)
				if err != nil {
					log.Println("err", err)
					continue
				}

				// increase the number of concurrent clients count
				con_clients++
				syscall.SetNonblock(fd, true)

				// add this new TCP connection to be monitored (replaces EpollCtl ADD)
				clientEvent := syscall.Kevent_t{
					Ident:  uint64(fd),
					Filter: syscall.EVFILT_READ,
					Flags:  syscall.EV_ADD,
				}
				if _, err := syscall.Kevent(kqueueFD, []syscall.Kevent_t{clientEvent}, nil, nil); err != nil {
					log.Fatal(err)
				}
			} else {
				comm := core.FDComm{Fd: int(events[i].Ident)}
				cmd, err := readCommand(comm)
				if err != nil {
					syscall.Close(int(events[i].Ident))
					con_clients--
					continue
				}
				respond(cmd, comm)
			}
		}
	}
}
