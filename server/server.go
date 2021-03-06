package main

import (
	"bus_sockets/buses"
	"bus_sockets/services"
	"context"
	"flag"
	"github.com/gorilla/websocket"
	"github.com/streadway/amqp"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var upgrader = websocket.Upgrader{}

var busesInfoToSend = sync.Map{}

type CoordsData struct {
	MsgType string `json:"msgType"`
	Data    *WindowCoords
}

type UserConnection struct {
	ws     *websocket.Conn
	coords *WindowCoords
}

func (u *UserConnection) isBusInside(
	lat float64,
	lng float64,
) bool {
	if u.coords == nil {
		return false
	}
	latInside := u.coords.SouthLat <= lat && lat <= u.coords.NorthLat
	lngInside := u.coords.WestLng <= lng && lng <= u.coords.EastLng
	return latInside && lngInside
}

type WindowCoords struct {
	EastLng  float64 `json:"east_lng"`
	NorthLat float64 `json:"north_lat"`
	SouthLat float64 `json:"south_lat"`
	WestLng  float64 `json:"west_lng"`
}

type MainHandler struct {
	Ctx      context.Context
	RabbitCh <-chan amqp.Delivery
}

func (m *MainHandler) sendBusInfo(
	userConnection *UserConnection,
	busesData *buses.BusesData,
) error {
	select {
	case <-m.Ctx.Done():
		_ = userConnection.ws.SetWriteDeadline(time.Now().Add(time.Second * 1))
		err := userConnection.ws.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			return err
		}
		select {
		case <-time.After(time.Second):
		}
		return nil
	default:
		var busesRouteData []buses.BusRouteData
		busesInfoToSend.Range(func(key, value interface{}) bool {
			val, ok := value.(buses.BusRouteData)
			if !ok {
				return false
			}
			if userConnection.isBusInside(val.Lat, val.Lng) {
				busesRouteData = append(busesRouteData, val)
			}

			return true
		})
		if busesRouteData != nil {
			busesData.Buses = busesRouteData
			_ = userConnection.ws.SetWriteDeadline(time.Now().Add(time.Second * 2))
			err := userConnection.ws.WriteJSON(&busesData)
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func (m *MainHandler) listenBrowser(
	ws *websocket.Conn,
	userConnection *UserConnection,
) {
	for {
		select {
		case <-m.Ctx.Done():
			return
		default:
			var coordsData CoordsData
			err := ws.ReadJSON(&coordsData)
			if err != nil {
				return
			}
			userConnection.coords = coordsData.Data
		}
	}
}

func (m *MainHandler) wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	userConnection := UserConnection{
		ws: ws,
	}

	defer ws.Close()
	busesData := buses.BusesData{
		MsgType: "Buses",
	}

	wg := sync.WaitGroup{}
	defer wg.Wait()
	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		m.listenBrowser(ws, &userConnection)
	}(&wg)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := m.sendBusInfo(&userConnection, &busesData)
		if err != nil {
			return
		}
		<-ticker.C
	}
}

func getBusInfoFromRabbit(ctx context.Context, msgs <-chan amqp.Delivery) error {
	for d := range msgs {

		select {
		case <-ctx.Done():
			return nil
		default:
			busData := buses.BusRouteData{}
			err := busData.UnmarshalJSON(d.Body)
			log.Printf(" [X] Recieve %s\n", busData.BusID)
			if err != nil {
				return err
			}
			busesInfoToSend.Store(busData.BusID, busData)
		}
	}
	return nil
}

var rabbitHost = flag.String("r_host", "127.0.0.1", "RabbitMQ host")
var rabbitPort = flag.Int("r_port", 5672, "RabbitMQ port")
var rabbitLogin = flag.String("r_login", "rabbitmq", "RabbitMQ login")
var rabbitPass = flag.String("r_pass", "rabbitmq", "RabbitMQ password")

func main() {

	ctx, cancel := context.WithCancel(context.Background())
	shutDownCh := make(chan os.Signal, 1)
	done := make(chan bool, 1)

	signal.Notify(shutDownCh, syscall.SIGINT, syscall.SIGTERM)
	rabbit := services.Rabbit{}
	go func() {
		sig := <-shutDownCh
		cancel()
		time.Sleep(1 * time.Second)
		log.Printf("Shutdown by signal: %s", sig)
		rabbit.Stop()
		done <- true
	}()

	rabbit.Start(
		*rabbitHost,
		*rabbitLogin,
		*rabbitPass,
		*rabbitPort,
	)
	messagesCh := rabbit.GetConsumeChan()
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		err := getBusInfoFromRabbit(ctx, messagesCh)
		if err != nil {
			log.Fatal(err)
		}
	}(&wg)

	go func() {
		mHandler := MainHandler{Ctx: ctx, RabbitCh: messagesCh}
		http.HandleFunc("/ws", mHandler.wsHandler)
		log.Println("Start ws server as :8000")
		http.ListenAndServe(":8000", nil)
	}()

	wg.Wait()
	<-done
}
