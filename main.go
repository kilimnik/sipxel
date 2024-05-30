package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgox"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/icholy/digest"
)

func main() {
	inter := flag.String("h", "localhost", "My interface ip or hostname")
	dst := flag.String("srv", "127.0.0.1:5060", "Destination")
	tran := flag.String("t", "udp", "Transport")
	username := flag.String("u", "alice", "SIP Username")
	password := flag.String("p", "alice", "Password")
	callNumber := flag.String("call", "", "Number to call")
	flag.Parse()

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	log.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.StampMicro,
	}).With().Timestamp().Logger().Level(zerolog.InfoLevel)

	if lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil && lvl != zerolog.NoLevel {
		log.Logger = log.Logger.Level(lvl)
	}

	// Setup UAC
	ua, err := sipgo.NewUA()
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup user agent")
	}

	_, err = registerClient(username, password, dst, inter, tran, ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to register client")
	}

	log.Info().Msg("Client registered")

	if *callNumber != "" {
		testDial(callNumber, dst, ua)
	} else {
		proxy(ua, username)
	}
}

func testDial(callNumber *string, dst *string, ua *sipgo.UserAgent) {
	recipient := sip.Uri{User: *callNumber, Host: *dst, Headers: sip.NewParams()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	phone := sipgox.NewPhone(ua)
	dialog, err := phone.Dial(ctx, recipient, sipgox.DialOptions{})
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to dial")
	}
	defer dialog.Close()

	sequencer := rtp.NewFixedSequencer(1)

	go func() {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         false,
				PayloadType:    0,
				SequenceNumber: sequencer.NextSequenceNumber(),
				Timestamp:      20, // Figure out how to do timestamps
				SSRC:           111222,
			},
			Payload: []byte("1234567890"),
		}

		if err := dialog.WriteRTP(pkt); err != nil {
			log.Error().Err(err).Msg("Fail to send RTP")
			return
		}

		dialog.Hangup(ctx)
	}()
}

func proxy(ua *sipgo.UserAgent, username *string) {
	stateMapMutex := &sync.RWMutex{}
	stateMap := map[sip.CallIDHeader]*Pixel{}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup server handle")
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		callID := req.CallID()

		to := req.To()
		user := to.Address.User[len(*username):] + "00000000"
		x, _ := strconv.Atoi(user[0:4])
		y, _ := strconv.Atoi(user[4:8])

		stateMapMutex.Lock()
		stateMap[*callID] = &Pixel{
			x,
			y,
			make(map[int]int),
		}
		stateMapMutex.Unlock()

		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		// Send response
		tx.Respond(res)

		// select {
		// case <-tx.Done():
		// 	return
		// }
	})

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		callID := req.CallID()

		stateMapMutex.RLock()
		state := stateMap[*callID]
		stateMapMutex.RUnlock()

		con, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to connect to pixelflut server")
		}

		keys := make([]int, 0, len(state.colors))
		for k := range state.colors {
			keys = append(keys, k)
		}
		sort.Ints(keys)

		color := 0
		for _, k := range keys {
			color = color*10 + state.colors[k]
		}

		r := (color / (1000 * 1000)) % 256
		g := ((color / 1000) % 1000) % 256
		b := ((color) % 1000) % 256

		fmt.Printf("DONE %v, %d %d => %d %d %d\n", callID, state.x, state.y, r, g, b)

		con.Write([]byte(fmt.Sprintf("PX %d %d %02x%02x%02x\n", state.x, state.y, r, g, b)))
		con.Close()
	})

	srv.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		fmt.Printf("%v\n", req.Body())
		signalStr := string(req.Body()[7:])
		signal, _ := strconv.Atoi(signalStr[:len(signalStr)-2])

		key := req.CallID()

		stateMapMutex.RLock()
		if val, ok := stateMap[*key]; ok {
			val.colors[int(req.CSeq().SeqNo)] = signal
		}
		stateMapMutex.RUnlock()
	})

	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	<-sig
}

func registerClient(username *string, password *string, dst *string, inter *string, tran *string, ua *sipgo.UserAgent) (*sipgo.Client, error) {
	client, err := sipgo.NewClient(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup client handle")
		return nil, err
	}
	defer client.Close()

	// Create basic REGISTER request structure
	recipient := &sip.Uri{}
	sip.ParseUri(fmt.Sprintf("sip:%s@%s", *username, *dst), recipient)
	req := sip.NewRequest(sip.REGISTER, *recipient)
	req.AppendHeader(
		sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s>", *username, *inter)),
	)
	req.SetTransport(strings.ToUpper(*tran))

	// Send request and parse response
	// req.SetDestination(*dst)
	log.Info().Msg(req.StartLine())
	ctx := context.Background()
	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to create transaction")
		return nil, err
	}
	defer tx.Terminate()

	res, err := getResponse(tx)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to get response")
		return nil, err
	}

	log.Info().Int("status", int(res.StatusCode)).Msg("Received status")

	if res.StatusCode == 401 {
		// Get WwW-Authenticate
		wwwAuth := res.GetHeader("WWW-Authenticate")
		chal, err := digest.ParseChallenge(wwwAuth.Value())
		if err != nil {
			log.Fatal().Str("wwwauth", wwwAuth.Value()).Err(err).Msg("Fail to parse challenge")
			return nil, err
		}

		// Reply with digest
		cred, _ := digest.Digest(chal, digest.Options{
			Method:   req.Method.String(),
			URI:      recipient.Host,
			Username: *username,
			Password: *password,
		})

		newReq := sip.NewRequest(sip.REGISTER, *recipient)
		newReq.AppendHeader(
			sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s>", *username, *inter)),
		)
		newReq.SetTransport(strings.ToUpper(*tran))
		newReq.AppendHeader(sip.NewHeader("Authorization", cred.String()))

		ctx := context.Background()
		tx, err := client.TransactionRequest(ctx, newReq)
		if err != nil {
			log.Fatal().Err(err).Msg("Fail to create transaction")
			return nil, err
		}
		defer tx.Terminate()

		res, err = getResponse(tx)
		if err != nil {
			log.Fatal().Err(err).Msg("Fail to get response")
			return nil, err
		}

		contact := res.Contact()
		expires, _ := contact.Params.Get("expires")
		expiresInt, _ := strconv.Atoi(expires)
		fmt.Printf("Expires: %s\n", expires)

		ticker := time.NewTicker(time.Duration(expiresInt-100) * time.Second)
		quit := make(chan struct{})
		go func() {
			for {
				select {
				case <-ticker.C:
					registerClient(username, password, dst, inter, tran, ua)
				case <-quit:
					ticker.Stop()
					return
				}
			}
		}()
	}

	if res.StatusCode != 200 {
		log.Fatal().Msg("Fail to register")
	}

	return client, nil
}

func getResponse(tx sip.ClientTransaction) (*sip.Response, error) {
	select {
	case <-tx.Done():
		return nil, fmt.Errorf("transaction died")
	case res := <-tx.Responses():
		for {
			if res.StatusCode == 100 {
				res = <-tx.Responses()
			} else {
				break
			}
		}

		return res, nil
	}
}

type Pixel struct {
	x      int
	y      int
	colors map[int]int
}
