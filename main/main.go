package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	exclusion "github.com/Moedefeis/MutualExclusion/grpc"
	"google.golang.org/grpc"
)

var empty = &exclusion.Empty{}
var accessCount = 0
var doneMap = make(map[int32]bool)
var allDone = make(chan bool)
var done = false
var token sync.Mutex

func main() {
	ownPort, _ := strconv.Atoi(os.Args[1])
	log.Printf("%d", ownPort)
	ports := readPorts(ownPort)
	rand.Seed(int64(ownPort))
	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", ownPort))
	if err != nil {
		log.Fatalf("Failed to listen on port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startPort := startPort(ownPort, ports)

	p := &peer{
		id:          int32(ownPort),
		nextPort:    calculateNextPort(ownPort, ports),
		wantsAccess: false,
		stopped:     true,
		stoppedPort: startPort,
		hasToken:    startPort == int32(ownPort),
		passToken:   make(chan bool),
		clients:     make(map[int32]exclusion.ExclusionClient),
		ctx:         ctx,
	}
	log.Printf("%d", p.nextPort)

	if p.hasToken {
		clearCriticalSection()
	}

	server := grpc.NewServer()
	exclusion.RegisterExclusionServer(server, p)

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatalf("failed to server %v", err)
		}
	}()

	for _, port := range ports {
		doneMap[int32(port)] = false
		if port == ownPort {
			continue
		}

		log.Printf("Trying to dial: %d\n", port)
		conn, err := grpc.Dial(fmt.Sprintf(":%v", port), grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			log.Fatalf("Could not connect: %v", err)
		}
		defer conn.Close()
		c := exclusion.NewExclusionClient(conn)
		p.clients[int32(port)] = c
	}

	go p.delayBeforeNextAccessInsentive()

	for !done {
		select {
		case <-p.passToken:
			log.Printf("Passed token")
			go p.clients[p.nextPort].GiveToken(p.ctx, empty)
		case <-allDone:
			done = true
		}
	}
	log.Printf("Exiting")
}

func (p *peer) delayBeforeNextAccessInsentive() {
	delay := rand.Intn(50)
	time.Sleep(time.Duration(delay) * time.Millisecond)
	p.wantsAccess = true
	log.Printf("Wants access")
	if p.stopped {
		if p.hasToken {
			p.StartPassing(p.ctx, empty)
		} else {
			log.Printf("Requested passing: %d", p.stoppedPort)
			p.clients[p.stoppedPort].StartPassing(p.ctx, empty)
		}
	}
}

type peer struct {
	exclusion.UnimplementedExclusionServer

	id          int32
	nextPort    int32
	stoppedPort int32
	stopped     bool //Its true if there is a possibility it stopped
	wantsAccess bool
	hasToken    bool
	passToken   chan bool
	clients     map[int32]exclusion.ExclusionClient
	ctx         context.Context
}

func (p *peer) GiveToken(ctx context.Context, empty *exclusion.Empty) (*exclusion.Empty, error) {
	token.Lock()
	log.Printf("Got token")
	p.stopped = false
	p.hasToken = true
	if p.wantsAccess && accessCount < 5 {
		log.Printf("Is accessing")
		p.accessCriticalSection()
		p.wantsAccess = false
		accessCount++
		if accessCount == 5 {
			log.Printf("done")
			go p.pingAllDone()
		} else {
			go p.delayBeforeNextAccessInsentive()
		}
		p.stopped = p.requestToStopPassingToken()
	}
	token.Unlock()
	if !p.stopped {
		p.safePass()
	}
	return empty, nil
}

func (p *peer) requestToStopPassingToken() bool {
	log.Printf("Requested stop")
	for _, peer := range p.clients {
		response, err := peer.StopPassingRequest(p.ctx, &exclusion.PeerId{Port: p.id})
		if err != nil {
			log.Printf(err.Error())
		} else if !response.AllowStop {
			log.Printf("	not allowed")
			return false
		}
	}
	log.Printf("	allowed")
	return true
}

func (p *peer) safePass() {
	token.Lock()
	if p.hasToken {
		p.hasToken = false
		p.passToken <- true
	}
	token.Unlock()
}

func (p *peer) StopPassingRequest(ctx context.Context, sender *exclusion.PeerId) (*exclusion.Response, error) {
	allowStop := !p.wantsAccess
	if allowStop {
		p.stopped = true
		p.stoppedPort = sender.Port
	}
	return &exclusion.Response{AllowStop: allowStop}, nil
}

func (p *peer) StartPassing(ctx context.Context, empty *exclusion.Empty) (*exclusion.Empty, error) {
	if p.hasToken && p.stopped {
		p.stopped = false
		log.Printf("Received pass request")
		p.safePass()
	}
	return empty, nil
}

func (p *peer) Done(ctx context.Context, sender *exclusion.PeerId) (*exclusion.Empty, error) {
	doneMap[sender.Port] = true
	checkIfAllDone()
	return empty, nil
}

func calculateNextPort(ownPort int, ports []int) int32 {
	exists := false //If there exists a port larger than its own port
	min := 999999   //Smallest port
	value := min    //Smallest port greater than its own port
	for _, port := range ports {
		if port < min {
			min = port
		} else if port > ownPort && port < value {
			value = port
			exists = true
		}
	}
	if exists {
		return int32(value)
	} else {
		return int32(min)
	}
}

func startPort(ownPort int, ports []int) int32 {
	min := 999999
	for _, port := range ports {
		if port < min {
			min = port
		}
	}
	return int32(min)
}

func (p *peer) accessCriticalSection() {
	accessedText := fmt.Sprintf("Peer %d accessed critical section\n", p.id)
	file, err := os.OpenFile("Critical Section.txt", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		log.Fatalf("Error trying to open file: %v", err)
	}
	defer file.Close()

	_, err = file.WriteString(accessedText)
	if err != nil {
		log.Fatalf("Error trying to write file: %v", err)
	}
}

func readPorts(ownPort int) []int {
	file, err := os.Open("ports.txt")
	if err != nil {
		log.Fatalf("Error trying to open file: %v", err)
	}
	defer file.Close()

	ports := make([]int, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		port, _ := strconv.Atoi(scanner.Text())
		ports = append(ports, port)
	}
	return ports
}

func clearCriticalSection() {
	if err := os.Truncate("Critical Section.txt", 0); err != nil {
		log.Fatalf("Failed to truncate: %v", err)
	}
}

func checkIfAllDone() {
	for _, isDone := range doneMap {
		if !isDone {
			return
		}
	}
	allDone <- true
}

func (p *peer) pingAllDone() {
	doneMap[p.id] = true
	for _, peer := range p.clients {
		peer.Done(p.ctx, &exclusion.PeerId{Port: p.id})
	}
	checkIfAllDone()
}
