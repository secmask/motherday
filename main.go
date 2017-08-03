package main

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	defaultDiskPerPlayer        = 100
	defaultRoundDurationSeconds = 60
	playing                     = "playing"
	waitingJoin                 = "waiting_join"
	stop                        = "stop"

	evGameStopped  = "game_stop"     // game stop because admin stop it, or timeout
	evGameWin      = "game_win"      // game stop because all of disc has clean
	evGameContinue = "game_continue" // there're more discs left
	evGameStart    = "game_start"    // a new game just get started, client should send 'client_request_more' to pull new disc
)

var (
	upgrader          = websocket.Upgrader{} // use default options
	broadcastChannels = NewBroadcastChannels()
	gameChannel       = broadcastChannels.GetOrCreate("the-only")

	discsPerPlayer       int64 // this is not fixed number of disk deliver to one player, just help to calculate total disk for a round
	state                = stop
	roundDurationSeconds int64
	gameRound            = GameRoundReport{JoinedPlayers: make(map[string]Player)}
	timer                *time.Timer
)

type GameRoundReport struct {
	TotalDisc         int64
	DiscLeft          int64
	PlayedTime        time.Duration
	JoinedPlayerCount int64
	JoinedPlayers     map[string]Player
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
	Type string
	Data map[string]interface{}
}

type receiver0 struct {
	c chan interface{}
}

func (r receiver0) Chan() chan interface{} {
	return r.c
}

func eventHandler(context echo.Context) error {
	c, err := upgrader.Upgrade(context.Response(), context.Request(), nil)
	if err != nil {
		log.Printf("upgrade:", err)
		return err
	}
	defer c.Close()

	outgoingChannel := make(chan interface{}, 32)
	gameChannel.AddReceiver(&receiver0{outgoingChannel})
	go func() {
		for {
			m := ClientMessage{}
			err := c.ReadJSON(&m)
			if err != nil {
				log.Printf("read json error %v\n", err)
				break
			}
			switch m.Type {
			case "client_request_more":
				if state == stop { // game has ended, client should receive game_done recently
					err = c.WriteJSON(&ServerMessage{
						Type: evGameStopped,
					})
					if err != nil {
						break
					}
					log.Printf("client_request_more on stopped game, ignore\n")
					continue
				}
				if discLeft := atomic.AddInt64(&gameRound.DiscLeft, -1); discLeft <= 0 {
					state = stop
					gameChannel.Send(&ServerMessage{
						Type: evGameWin,
					})
					timer.Stop()
					log.Printf("game end because no disc left\n")
				} else { // there're discs left, continue
					err = c.WriteJSON(&ServerMessage{
						Type: evGameContinue,
					})
					if err != nil {
						break
					}
					log.Printf("player %s just cleanup a disk, number left %d\n", m.ClientID, discLeft)
				}
			}
		}
	}()

	for {
		select {
		case event := <-outgoingChannel:
			if err := c.WriteJSON(event); err != nil {
				break
			}
		}
	}

	return nil
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
	fmt.Fprintf(context.Response(), "round_duration = %ds\n", roundDurationSeconds)

	return nil
}

// gameJoinHandler process join game round for a player
func gameJoinHandler(context echo.Context) error {

	if state != waitingJoin {
		return context.String(http.StatusLocked, "can't join during play")
	}

	playerID := context.Request().FormValue("player_id")
	gameRound.JoinedPlayers[playerID] = Player{
		ID: playerID,
	}
	count := atomic.AddInt64(&gameRound.JoinedPlayerCount, 1)
	context.String(http.StatusOK, "OK")
	log.Printf("%s just join, we have %d player now\n", playerID, count)

	return nil
}

func gameNewRoundHandler(context echo.Context) error {
	switch state {
	case playing:
		state = stop
		gameChannel.Send(&ServerMessage{
			Type: evGameStopped,
		})
		timer.Stop()
	case waitingJoin:
	case stop:
	}

	gameRound = GameRoundReport{
		JoinedPlayers: make(map[string]Player),
	}

	gameRound.TotalDisc = discsPerPlayer * gameRound.JoinedPlayerCount
	gameRound.DiscLeft = gameRound.TotalDisc

	state = waitingJoin
	return nil
}

func gameStartHandler(context echo.Context) error {
	if state != waitingJoin {
		return context.String(http.StatusBadRequest, "No game created, create a new one first")
	}

	gameRound = GameRoundReport{
		JoinedPlayers: make(map[string]Player),
	}

	gameRound.TotalDisc = discsPerPlayer * gameRound.JoinedPlayerCount
	gameRound.DiscLeft = gameRound.TotalDisc

	state = playing
	timer = time.AfterFunc(time.Duration(roundDurationSeconds)*time.Second, func() {
		state = stop
		gameChannel.Send(&ServerMessage{
			Type: evGameStopped,
		})
		log.Printf("game stop because out of time\n")
	})
	log.Printf("game start with %d discs and %d player\n", gameRound.TotalDisc, gameRound.JoinedPlayerCount)
	gameChannel.Send(&ServerMessage{
		Type: evGameStart,
	})
	return nil
}

func main() {
	e := echo.New()
	e.Static("/content", "./content")
	e.GET("/game_event", eventHandler)
	e.GET("/game_join", gameJoinHandler)
	e.GET("/game_setup", gameSetupHandler)
	e.GET("/game_start", gameStartHandler)
	e.GET("/game_new", gameNewRoundHandler)
	err := e.Start(":9090")
	log.Panic(err)
}
