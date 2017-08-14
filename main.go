package main

import (
	"flag"
	"fmt"
	l "log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
)

const (
	defaultDiskPerPlayer        = 30
	defaultRoundDurationSeconds = 30
	playing                     = "playing"
	waitingJoin                 = "waiting_join"
	stop                        = "stop"

	evGameNew         = "game_new"
	evGameStopped     = "game_stop"         // game stop because admin stop it, or timeout
	evGameWin         = "game_win"          // game stop because all of disc has clean
	evGameContinue    = "game_continue"     // there're more discs left
	evGameWaitingJoin = "game_waiting_join" // waiting for join game
	evLater           = "game_later"
	evGameStart       = "game_start"         // a new game just get started, client should send 'client_request_more' to pull new disc
	evPlayerJoined    = "game_player_joined" // announce that a new player just joined
	evProgress        = "game_progress"      // send out playing statistic

)

const (
	clientJoinGame  = "join_game"
	clientFetchDisc = "fetch_disc"
)

var (
	log               = l.New(os.Stdout, "", l.LstdFlags|l.Lshortfile)
	upgrader          = websocket.Upgrader{} // use default options
	broadcastChannels = NewBroadcastChannels()
	gameChannel       = broadcastChannels.GetOrCreate("the-only")

	discsPerPlayer       int64 = defaultDiskPerPlayer // this is not fixed number of disk for one player, just help to calculate total disk for a round
	roundDurationSeconds int64 = defaultRoundDurationSeconds
	gameState            *GameState
	gsLock               sync.Mutex
	gameStartingTicker   *time.Ticker
)

type GameState struct {
	TotalDisc         int64             `json:"total_disc"`
	DiscLeft          int64             `json:"disc_left"`
	CleanItem         int64             `json:"clean_item"`
	StartTime         time.Time         `json:"start_time"`
	SecondsLeft       int64             `json:"seconds_left"`
	PlayedTime        time.Duration     `json:"played_time"`
	PlayedTimeSeconds float64           `json:"played_time_seconds"`
	JoinedPlayers     map[string]Player `json:"joined_players"`
	State             string
}

type Player struct {
	ID string
}

type ClientMessage struct {
	Type     string                 `json:"type"`
	ClientID string                 `json:"client_id"`
	Data     map[string]interface{} `json:"data"`
}

type ServerMessage struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

type receiver0 struct {
	c chan interface{}
}

func (r receiver0) Chan() chan interface{} {
	return r.c
}

func gameSetupHandler(context echo.Context) error {
	var err error

	dPerPlayer := context.FormValue("disc_per_player")
	discsPerPlayer, err = strconv.ParseInt(dPerPlayer, 10, 0)
	if err != nil {
		log.Printf("can't parse disc_per_player: %v\n", err)
		discsPerPlayer = defaultDiskPerPlayer
	}

	roundTime := context.FormValue("round_time")
	roundDurationSeconds, err = strconv.ParseInt(roundTime, 10, 0)
	if err != nil || roundDurationSeconds == 0 {
		roundDurationSeconds = defaultRoundDurationSeconds
	}

	fmt.Fprintf(context.Response(), "disc_per_player = %d\n", discsPerPlayer)
	fmt.Fprintf(context.Response(), "round_time = %ds\n", roundDurationSeconds)

	return nil
}

func startCountingStartGame() {
	gameStartingTicker = time.NewTicker(time.Second)
	var timeCountDown int64 = 15
	for range gameStartingTicker.C {
		gameChannel.Send(ServerMessage{
			Type: evGameWaitingJoin,
			Data: map[string]interface{}{
				"state":   gameState,
				"seconds": timeCountDown,
			},
		})
		log.Printf("waiting for player join %d\n", timeCountDown)
		timeCountDown = timeCountDown - 1
		if timeCountDown == -1 {
			gsLock.Lock()
			gameState.TotalDisc = int64(len(gameState.JoinedPlayers)) * discsPerPlayer
			gameState.StartTime = time.Now()
			gameState.DiscLeft = gameState.TotalDisc
			gameState.SecondsLeft = roundDurationSeconds
			gameState.State = playing
			gsLock.Unlock()
			gameChannel.Send(ServerMessage{
				Type: evGameStart,
				Data: map[string]interface{}{
					"state": gameState,
				},
			})
			break
		}
	}
	timeCountDown = gameState.SecondsLeft
	for range gameStartingTicker.C {
		gsLock.Lock()
		gameState.PlayedTime = time.Now().Sub(gameState.StartTime)
		gameState.PlayedTimeSeconds = gameState.PlayedTime.Seconds()
		gameState.SecondsLeft = timeCountDown
		gsLock.Unlock()

		log.Printf("game end in %d\n", timeCountDown)
		if timeCountDown == -1 {
			gsLock.Lock()
			gameState.State = stop
			gsLock.Unlock()
			gameChannel.Send(ServerMessage{
				Type: evGameStopped,
				Data: map[string]interface{}{
					"state":   gameState,
					"seconds": 0,
				},
			})
			break
		} else {
			gameChannel.Send(ServerMessage{
				Type: evProgress,
				Data: map[string]interface{}{
					"state":   gameState,
					"seconds": timeCountDown,
				},
			})
		}
		timeCountDown = timeCountDown - 1
	}
}

func gameJoin(context echo.Context) error {
	c, err := upgrader.Upgrade(context.Response(), context.Request(), nil)
	if err != nil {
		log.Printf("upgrade:", err)
		return err
	}
	done := make(chan struct{})

	outgoingChannel := make(chan interface{}, 32)
	eventReceiver := &receiver0{outgoingChannel}
	clientID := ""
	gameChannel.AddReceiver(eventReceiver)
	defer func() {
		gameChannel.RemoveReceiver(eventReceiver)
		c.Close()
		gsLock.Lock()
		if gameState != nil && clientID != "" {
			delete(gameState.JoinedPlayers, clientID)
		}
		gsLock.Unlock()
	}()

	go func() {
		defer close(done)
		for {
			m := ClientMessage{}
			err := c.ReadJSON(&m)
			if err != nil {
				log.Printf("read json error %v\n", err)
				break
			}
			//log.Printf("got: %+v\n", m)

			switch m.Type {
			case clientJoinGame:
				var newGame bool
				gsLock.Lock()
				if gameState == nil || gameState.State == stop {
					gameState = &GameState{JoinedPlayers: make(map[string]Player), State: waitingJoin}
					newGame = true
				}
				if gameState.State != waitingJoin {
					log.Printf("state %s not allow join\n", gameState.State)
					c.WriteJSON(ServerMessage{
						Type: evLater,
						Data: map[string]interface{}{
							"state": gameState,
						},
					})
					continue
				}
				gameState.JoinedPlayers[m.ClientID] = Player{m.ClientID}
				gsLock.Unlock()
				clientID = m.ClientID
				if newGame {
					gameChannel.Send(ServerMessage{
						Type: evGameNew,
					})
					go startCountingStartGame()
				}
				gameChannel.Send(ServerMessage{
					Type: evPlayerJoined,
					Data: map[string]interface{}{
						"state": gameState,
					},
				})
			case clientFetchDisc:
				count := m.Data["count"]
				gsLock.Lock()
				if gameState == nil || gameState.State != playing {
					gsLock.Unlock()
					continue
				}
				if count == "1" {
					if gameState != nil && gameState.State == playing {
						gameState.DiscLeft = gameState.DiscLeft - 1
						gameState.CleanItem = gameState.CleanItem + 1
					}
				}
				if gameState.DiscLeft == 0 {
					if gameStartingTicker != nil {
						gameStartingTicker.Stop()
					}
					gameState.State = stop
					gameState.PlayedTime = time.Now().Sub(gameState.StartTime)
					gameState.PlayedTimeSeconds = gameState.PlayedTime.Seconds()
					gameChannel.Send(ServerMessage{
						Type: evGameWin,
						Data: map[string]interface{}{
							"state": gameState,
						},
					})
					log.Printf("game win in %v\n", gameState.PlayedTime.Seconds())
				} else {
					c.WriteJSON(ServerMessage{
						Type: evGameContinue,
						Data: map[string]interface{}{
							"state": gameState,
						},
					})
				}
				gsLock.Unlock()
			}
			log.Println("current game: ", gameState.DiscLeft, gameState.SecondsLeft)
		}
	}()
outside:
	for {
		select {
		case event := <-outgoingChannel:
			if err := c.WriteJSON(event); err != nil {
				break outside
			}
		case <-done:
			break outside
		}
	}

	return nil
}

func main() {
	var bindAddr = ":9090"
	flag.StringVar(&bindAddr, "bind-address", ":9090", "bind address")
	flag.Parse()
	e := echo.New()
	e.Static("/content", "./content")
	e.GET("/game_join", gameJoin)
	e.GET("/game_setup", gameSetupHandler)
	go func() {
		http.ListenAndServe(":5555", nil)
	}()
	err := e.Start(bindAddr)
	log.Panic(err)
}
