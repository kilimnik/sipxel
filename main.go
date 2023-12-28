package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

	_, err = registerClient(username, password, dst, inter, tran, ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to register client")
	}

	log.Info().Msg("Client registered")

	stateMapMutex := &sync.RWMutex{}
	stateMap := map[sip.CallIDHeader]*Pixel{}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup server handle")
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		callID, _ := req.CallID()

		to, _ := req.To()
		user := to.Address.User[len(*username):] + "00000000"
		x, _ := strconv.Atoi(user[0:4])
		y, _ := strconv.Atoi(user[4:8])

		stateMapMutex.Lock()
		stateMap[*callID] = &Pixel{
			x,
			y,
			0,
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
		callID, _ := req.CallID()

		stateMapMutex.RLock()
		state := stateMap[*callID]
		stateMapMutex.RUnlock()

		con, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to connect to pixelflut server")
		}

		r := (state.color / (1000 * 1000)) % 256
		g := ((state.color / 1000) % 1000) % 256
		b := ((state.color) % 1000) % 256

		fmt.Printf("DONE %v, %d %d => %d %d %d\n", callID, state.x, state.y, r, g, b)

		con.Write([]byte(fmt.Sprintf("PX %d %d %02x%02x%02x\n", state.x, state.y, r, g, b)))
		con.Close()
	})

	srv.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		signalStr := string(req.Body()[7:])
		signal, _ := strconv.Atoi(signalStr[:len(signalStr)-2])

		key, _ := req.CallID()

		stateMapMutex.RLock()
		if val, ok := stateMap[*key]; ok {
			val.color = (val.color * 10) + signal
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
	req := sip.NewRequest(sip.REGISTER, recipient)
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

		newReq := sip.NewRequest(sip.REGISTER, recipient)
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
		return res, nil
	}
}

type Pixel struct {
	x     int
	y     int
	color int
}
