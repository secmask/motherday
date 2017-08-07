package main

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	l "log"
	"net/http"
	"os"
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

	evGameStopped  = "game_stop"          // game stop because admin stop it, or timeout
	evGameWin      = "game_win"           // game stop because all of disc has clean
	evGameContinue = "game_continue"      // there're more discs left
	evGameStart    = "game_start"         // a new game just get started, client should send 'client_request_more' to pull new disc
	evPlayerJoined = "game_player_joined" // announce that a new player just joined
	evProgress     = "game_progress"      // send out playing statistic
)

var (
	log               = l.New(os.Stdout, "", l.LstdFlags|l.Lshortfile)
	upgrader          = websocket.Upgrader{} // use default options
	broadcastChannels = NewBroadcastChannels()
	gameChannel       = broadcastChannels.GetOrCreate("the-only")

	discsPerPlayer       int64 = defaultDiskPerPlayer // this is not fixed number of disk deliver to one player, just help to calculate total disk for a round
	state                      = stop
	roundDurationSeconds int64 = defaultRoundDurationSeconds
	gameRound                  = GameRoundReport{JoinedPlayers: make(map[string]Player)}
	ticker               *time.Ticker
)

type GameRoundReport struct {
	TotalDisc         int64
	DiscLeft          int64
	CleanItem         int64
	StartTime         time.Time
	SecondsLeft       int64
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

	outgoingChannel := make(chan interface{}, 32)
	eventReceiver := &receiver0{outgoingChannel}
	gameChannel.AddReceiver(eventReceiver)
	defer func() {
		c.Close()
		gameChannel.RemoveReceiver(eventReceiver)
	}()

	done := make(chan struct{})
	go func() {
		for {
			m := ClientMessage{}
			err := c.ReadJSON(&m)
			if err != nil {
				log.Printf("read json error %v\n", err)
				break
			}
			log.Printf("got: %+v\n", m)
			log.Printf("current game: %+v\n", gameRound)
			switch m.Type {
			case "client_request_more":
				count := m.Data["count"]
				if state == stop { // game has ended, client should receive game_stop recently
					err = c.WriteJSON(&ServerMessage{
						Type: evGameStopped,
						Data: map[string]interface{}{"reason": "eot"},
					})
					if err != nil {
						break
					}
					log.Printf("client_request_more on stopped game, ignore\n")
					break
				}
				if state == waitingJoin {
					log.Printf("client %s should wait for game start event, ignore\n", m.ClientID)
					break
				}
				var discLeft = atomic.LoadInt64(&gameRound.DiscLeft)
				if count == "1" {
					discLeft = atomic.AddInt64(&gameRound.DiscLeft, -1)
					atomic.AddInt64(&gameRound.CleanItem, 1)
				}
				if discLeft <= 0 {
					if discLeft == 0 {
						state = stop
						gameRound.PlayedTime = time.Since(gameRound.StartTime)
						gameChannel.Send(&ServerMessage{
							Type: evGameWin,
							Data: map[string]interface{}{
								"total_disc":   gameRound.TotalDisc,
								"clean_disc":   gameRound.CleanItem,
								"player_count": gameRound.JoinedPlayerCount,
								"played_time":  gameRound.PlayedTime.Seconds(),
								"state":        state,
							},
						})
						ticker.Stop()
						log.Printf("game end because all the disc has been cleaned\n")
					}
				} else { // there're discs left, continue
					err = c.WriteJSON(&ServerMessage{
						Type: evGameContinue,
					})
					if err != nil {
						log.Printf("player disconnected %s, %v\n", m.ClientID, err)
						break
					}
					log.Printf("player %s just cleanup a disk, number left %d\n", m.ClientID, discLeft)
				}
			}
		}
		close(done)
	}()

	for {
		select {
		case event := <-outgoingChannel:
			if err := c.WriteJSON(event); err != nil {
				break
			}
		case <-done:
			break
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
	fmt.Fprintf(context.Response(), "round_time = %ds\n", roundDurationSeconds)

	return nil
}

// gameJoinHandler process join game round for a player
func gameJoinHandler(context echo.Context) error {

	if state != waitingJoin {
		return context.String(http.StatusOK, "can't join during play")
	}

	playerID := context.Request().FormValue("player_id")
	gameRound.JoinedPlayers[playerID] = Player{
		ID: playerID,
	}
	count := atomic.AddInt64(&gameRound.JoinedPlayerCount, 1)
	context.String(http.StatusOK, "OK")
	log.Printf("%s just join, we have %d player now\n", playerID, count)

	ids := make([]string, 0, len(gameRound.JoinedPlayers))
	for p, _ := range gameRound.JoinedPlayers {
		ids = append(ids, p)
	}
	gameChannel.Send(&ServerMessage{
		Type: evPlayerJoined,
		Data: map[string]interface{}{"players": ids},
	})
	return nil
}

func gameNewRoundHandler(context echo.Context) error {
	switch state {
	case playing:
		ticker.Stop()
		fallthrough
	case waitingJoin:
		gameChannel.Send(&ServerMessage{
			Type: evGameStopped,
			Data: map[string]interface{}{"reason": "cancelled"},
		})
	case stop:
	}

	gameRound = GameRoundReport{
		JoinedPlayers: make(map[string]Player),
	}
	state = waitingJoin
	return nil
}

func gameStartHandler(context echo.Context) error {
	if state != waitingJoin {
		return context.String(http.StatusBadRequest, "No game created, create a new one first")
	}

	gameRound.TotalDisc = discsPerPlayer * gameRound.JoinedPlayerCount
	gameRound.DiscLeft = gameRound.TotalDisc
	gameRound.SecondsLeft = roundDurationSeconds
	gameRound.StartTime = time.Now()
	state = playing

	ticker = time.NewTicker(time.Second)
	log.Printf("game start with %d discs and %d player\n", gameRound.TotalDisc, gameRound.JoinedPlayerCount)
	gameChannel.Send(&ServerMessage{
		Type: evGameStart,
	})
	go func() {
		for range ticker.C {
			secondsLeft := atomic.AddInt64(&gameRound.SecondsLeft, -1)
			gameChannel.Send(&ServerMessage{
				Type: evProgress,
				Data: map[string]interface{}{
					"time_left":    secondsLeft,
					"total_disc":   gameRound.TotalDisc,
					"clean_disc":   gameRound.CleanItem,
					"player_count": gameRound.JoinedPlayerCount,
					"state":        state,
				},
			})
			if secondsLeft == 0 {
				state = stop
				gameRound.PlayedTime = time.Since(gameRound.StartTime)
				gameChannel.Send(&ServerMessage{
					Type: evGameStopped,
					Data: map[string]interface{}{"reason": "eot"},
				})
				ticker.Stop()
				break
			}
		}
	}()

	return nil
}

func mainScreenHandler(context echo.Context) error {
	c, err := upgrader.Upgrade(context.Response(), context.Request(), nil)
	if err != nil {
		log.Printf("upgrade:", err)
		return err
	}

	outgoingChannel := make(chan interface{}, 32)
	r := &receiver0{outgoingChannel}
	gameChannel.AddReceiver(r)
	defer func() {
		c.Close()
		gameChannel.RemoveReceiver(r)
	}()

	go func() {
		for {
			m := ClientMessage{}
			err := c.ReadJSON(&m)
			if err != nil {
				log.Printf("read json error %v\n", err)
				break
			}
			log.Printf("got: %+v\n", m)
			log.Printf("current game: %+v\n", gameRound)
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

func main() {
	e := echo.New()
	e.Static("/content", "./content")
	e.GET("/game_event", eventHandler)
	e.GET("/game_join", gameJoinHandler)
	e.GET("/game_setup", gameSetupHandler)
	e.GET("/game_start", gameStartHandler)
	e.GET("/game_main_screen", mainScreenHandler)
	e.GET("/game_new", gameNewRoundHandler)
	err := e.Start(":9090")
	log.Panic(err)
}
