package main

import (
	l "log"
	"os"
	"time"
)

var il = l.New(os.Stderr, "IL", l.LstdFlags|l.Lshortfile)

type Receiver interface {
	Chan() chan interface{}
}

type ReceiverFunc func() chan interface{}

func (r ReceiverFunc) Chan() chan interface{} {
	return r()
}

type addReceiver struct {
	receiver Receiver
}

type removeReceiver struct {
	receiver Receiver
}

type sendData struct {
	data interface{}
}

type Producer struct {
	receivers map[Receiver]struct{}
	events    chan interface{}
}

func NewProducer() *Producer {
	p := &Producer{
		receivers: make(map[Receiver]struct{}),
		events:    make(chan interface{}),
	}
	go p.run()
	return p
}

func (p *Producer) AddReceiver(e Receiver) {
	p.events <- &addReceiver{
		receiver: e,
	}
}

func (p *Producer) RemoveReceiver(e Receiver) {
	//debug.PrintStack()

	p.events <- &removeReceiver{
		receiver: e,
	}
}

func (p *Producer) Send(data interface{}) {
	p.events <- &sendData{
		data: data,
	}
}

func dropMessage(ev chan interface{}) {
	select {
	case <-ev:
		il.Println("detect slow consumer")
	default:
		break
	}
}
func (p *Producer) run() {
	for e := range p.events {
		switch e := e.(type) {
		case *addReceiver:
			p.receivers[e.receiver] = struct{}{}
		case *removeReceiver:
			delete(p.receivers, e.receiver)
		case *sendData:
			t := time.NewTimer(time.Millisecond * 100)
			for r := range p.receivers {
				t.Reset(time.Millisecond * 100)
				select {
				case r.Chan() <- e.data:
				case <-t.C:
					//dropMessage(r.Chan())
					//continue
					delete(p.receivers, r)
					il.Printf("close receiver due can not append item\n")
					close(r.Chan())
				}
			}
			t.Stop()
		}
	}
}

func (p *Producer) Close() {
	close(p.events)
}
