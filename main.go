package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
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

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup server handle")
	}

	var expiration = 0

	_, expiration, err = start(username, password, dst, inter, tran, srv.UserAgent, srv)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to start client")
	}

	for {
		if expiration == 0 {
			break
		}

		log.Info().Msg("Restarting client")
		ticker := time.NewTicker(time.Duration(expiration-100) * time.Second)
		quit := make(chan struct{})
		// go func() {
		for {
			select {
			case <-ticker.C:
				srv, err := sipgo.NewServer(ua)
				if err != nil {
					log.Fatal().Err(err).Msg("Fail to setup server handle")
				}

				_, expiration, err = start(username, password, dst, inter, tran, srv.UserAgent, srv)
				if err != nil {
					log.Fatal().Err(err).Msg("Fail to start client")
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
		// }()
	}
}

func start(username *string, password *string, dst *string, inter *string, tran *string, ua *sipgo.UserAgent, srv *sipgo.Server) (*sipgo.Client, int, error) {
	client, expiration, err := registerClient(username, password, dst, inter, tran, ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to register client")
		return nil, -1, err
	}

	log.Info().Msg("Client registered")
	fmt.Printf("Expires: %v\n", expiration)

	proxy(srv, username)

	return client, expiration, nil
}

func proxy(srv *sipgo.Server, username *string) {
	stateMap := sync.Map{}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		callID := req.CallID()

		_, ok := stateMap.Load(*callID)

		if ok {
			return
		}

		to := req.To()
		user := to.Address.User[len(*username):] + "00000000"
		x, _ := strconv.Atoi(user[0:4])
		y, _ := strconv.Atoi(user[4:8])

		pixel := &Pixel{
			x,
			y,
			make(map[int]int),
			make(map[uint32]bool),
		}
		pixel.seqs[req.CSeq().SeqNo] = true

		stateMap.Store(*callID, pixel)

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

		x, _ := stateMap.Load(*callID)
		if x == nil {
			return
		}
		state := x.(*Pixel)

		state.seqs[req.CSeq().SeqNo] = true

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

		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	srv.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		callID := req.CallID()

		x, _ := stateMap.Load(*callID)
		if x == nil {
			return
		}
		state := x.(*Pixel)

		_, ok := state.seqs[req.CSeq().SeqNo]
		if ok {
			return
		}
		state.seqs[req.CSeq().SeqNo] = true

		signalStr := string(req.Body()[7:])
		signal, _ := strconv.Atoi(signalStr[:len(signalStr)-2])
		state.colors[int(req.CSeq().SeqNo)] = signal

		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})
}

func registerClient(username *string, password *string, dst *string, inter *string, tran *string, ua *sipgo.UserAgent) (*sipgo.Client, int, error) {
	client, err := sipgo.NewClient(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup client handle")
		return nil, -1, err
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
		return nil, -1, err
	}
	defer tx.Terminate()

	res, err := getResponse(tx)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to get response")
		return nil, -1, err
	}

	var expiration = -1
	log.Info().Int("status", int(res.StatusCode)).Msg("Received status")

	if res.StatusCode == 401 {
		// Get WwW-Authenticate
		wwwAuth := res.GetHeader("WWW-Authenticate")
		chal, err := digest.ParseChallenge(wwwAuth.Value())
		if err != nil {
			log.Fatal().Str("wwwauth", wwwAuth.Value()).Err(err).Msg("Fail to parse challenge")
			return nil, -1, err
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
			return nil, -1, err
		}
		defer tx.Terminate()

		res, err = getResponse(tx)
		if err != nil {
			log.Fatal().Err(err).Msg("Fail to get response")
			return nil, -1, err
		}

		contact := res.Contact()
		expires, _ := contact.Params.Get("expires")
		expiresInt, _ := strconv.Atoi(expires)
		expiration = expiresInt
	}

	if res.StatusCode != 200 {
		log.Fatal().Msg("Fail to register")
	}

	return client, expiration, nil
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
	seqs   map[uint32]bool
}
