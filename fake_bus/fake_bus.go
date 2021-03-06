package main

import (
	"bus_sockets/buses"
	"bus_sockets/services"
	"context"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"
)

type BusInfo struct {
	Ctx  context.Context
	Info *buses.RouteInfo
}

type BusImitator struct {
	ctx            context.Context
	serverAddress  string
	refreshTimeout int
	routesCount    int
	busInfoChans   []chan *buses.BusRouteData
	busesPerRoute  int
	rabbit         services.Rabbit
}

func (b *BusImitator) initBusChans(
	readyWs chan struct{},
) {
	wg := sync.WaitGroup{}
	for i := 0; i < *chansCount; i++ {
		busInfoCh := make(chan *buses.BusRouteData, 0)
		b.busInfoChans = append(b.busInfoChans, busInfoCh)
		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()
			go b.spawnBusFromCh(busInfoCh)
		}(&wg)
	}
	readyWs <- struct{}{}
	wg.Wait()
}

func ReadBusDataFromFile(
	routesDir string,
	fileName string,
	busDataCh chan<- []byte,
) {
	fullPath := path.Join(routesDir, fileName)
	f, err := os.Open(fullPath)
	if err != nil {
		log.Printf("unable to open file: %s\n", err)
	}
	fileContent, err := ioutil.ReadAll(f)
	if err != nil {
		log.Printf("unable to read file: %s\n", err)
	}
	_ = f.Close()
	busDataCh <- fileContent
}

func (b *BusImitator) processRoutes(
	files []os.FileInfo,
	routesDir string,
) {
	wg := sync.WaitGroup{}
	for i := 0; i < b.routesCount; i++ {
		busDataChan := make(chan []byte, b.routesCount)
		fileInfo := files[i]
		rand.Seed(time.Now().UnixNano())
		busInfoChan := b.busInfoChans[rand.Intn(len(b.busInfoChans))]
		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()
			go ReadBusDataFromFile(routesDir, fileInfo.Name(), busDataChan)
		}(&wg)
		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()
			go b.spawnRoute(busDataChan, busInfoChan)
		}(&wg)
	}
	wg.Wait()
}

func (b *BusImitator) spawnBusFromCh(busInfoCh <-chan *buses.BusRouteData) {
	ticker := time.NewTicker(time.Duration(b.refreshTimeout) * time.Millisecond)
	defer func() {
		ticker.Stop()
	}()
	for {
		select {
		case busInfo := <-busInfoCh:
			msg, err := busInfo.MarshalJSON()
			if err != nil {
				log.Println("marshal error:", err)
				return
			}

			b.rabbit.SendData(msg)
			<-ticker.C
		case <-b.ctx.Done():

			return
		}
	}
}

func (b *BusImitator) sendBusToCh(
	InfoSender *BusInfo,
	busInfoCh chan<- *buses.BusRouteData,
	sendBusWg *sync.WaitGroup,
) {
	defer func() {
		sendBusWg.Done()
	}()
	busId := fmt.Sprintf("%s-%s", InfoSender.Info.Name, uuid.New().String()[:5])
	firstRun := true
	coords := InfoSender.Info.Coordinates
	for {

		if firstRun {
			randOffset := rand.Intn(len(coords) / 2)
			coords = coords[randOffset:]
		}
		for _, coord := range coords {
			busData := buses.BusRouteData{
				BusID: busId,
				Lat:   coord[0],
				Lng:   coord[1],
				Route: InfoSender.Info.Name,
			}
			select {
			case <-b.ctx.Done():
				return
			case busInfoCh <- &busData:
			}
		}

		if firstRun {
			firstRun = false
			coords = InfoSender.Info.Coordinates
		}
		for i := len(coords)/2 - 1; i >= 0; i-- {
			opp := len(coords) - 1 - i
			coords[i], coords[opp] = coords[opp], coords[i]
		}
	}
}

func (b *BusImitator) spawnRoute(
	busDataChan <-chan []byte,
	busInfoCh chan<- *buses.BusRouteData,
) {
	sendBusWg := sync.WaitGroup{}
	defer func() {
		sendBusWg.Wait()
	}()
	fileContent := <-busDataChan
	data := buses.RouteInfo{}
	err := data.UnmarshalJSON(fileContent)
	if err != nil {
		log.Printf("unable to unmarshal json: %s\n", err)
		return
	}
	InfoSender := BusInfo{
		Info: &data,
	}
	for i := 0; i < b.busesPerRoute; i++ {
		sendBusWg.Add(1)
		go b.sendBusToCh(&InfoSender, busInfoCh, &sendBusWg)
	}
}

var rabbitHost = flag.String("r_host", "127.0.0.1", "RabbitMQ host")
var rabbitPort = flag.Int("r_port", 5672, "RabbitMQ port")
var rabbitLogin = flag.String("r_login", "rabbitmq", "RabbitMQ login")
var rabbitPass = flag.String("r_pass", "rabbitmq", "RabbitMQ password")
var routesCount = flag.Int("routes", 20, "Count of routes")
var busesPerRoute = flag.Int("buses", 5, "Count of buses on one route")
var refreshTimeout = flag.Int("refresh", 100, "Refresh timeout (on milliseconds)")
var chansCount = flag.Int("chans", 10, "Count of parallel Golang chans for send bus data to rabbit")

func main() {
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	shutDownCh := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(shutDownCh, syscall.SIGINT, syscall.SIGTERM)
	imitator := BusImitator{
		ctx:            ctx,
		refreshTimeout: *refreshTimeout,
		routesCount:    *routesCount,
		busesPerRoute:  *busesPerRoute,
		busInfoChans:   []chan *buses.BusRouteData{},
	}
	go func() {
		sig := <-shutDownCh
		log.Printf("Shutdown by signal: %s", sig)
		cancel()
		imitator.rabbit.Stop()
		time.Sleep(1 * time.Second)
		done <- true
	}()

	dir, err := os.Getwd()
	if err != nil {
		log.Printf("unable to get current directory: %s\n", err)
		return
	}

	routesDir := path.Join(dir, "routes")
	files, err := ioutil.ReadDir(routesDir)

	if err != nil {
		log.Printf("unable to read routes directory: %s\n", err)
		return
	}

	readyRQ := make(chan struct{})
	imitator.rabbit.Start(
		*rabbitHost,
		*rabbitLogin,
		*rabbitPass,
		*rabbitPort,
	)
	go imitator.initBusChans(readyRQ)
	<-readyRQ

	fmt.Printf("Start imitator\n")
	imitator.processRoutes(files, routesDir)
	<-done

	fmt.Println("DONE OK")
}
