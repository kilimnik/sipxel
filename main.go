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
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgox"
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

	client, err := sipgo.NewClient(ua)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to setup client handle")
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
	}
	defer tx.Terminate()

	res, err := getResponse(tx)
	if err != nil {
		log.Fatal().Err(err).Msg("Fail to get response")
	}

	log.Info().Int("status", int(res.StatusCode)).Msg("Received status")
	if res.StatusCode == 401 {
		// Get WwW-Authenticate
		wwwAuth := res.GetHeader("WWW-Authenticate")
		chal, err := digest.ParseChallenge(wwwAuth.Value())
		if err != nil {
			log.Fatal().Str("wwwauth", wwwAuth.Value()).Err(err).Msg("Fail to parse challenge")
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
		}
		defer tx.Terminate()

		res, err = getResponse(tx)
		if err != nil {
			log.Fatal().Err(err).Msg("Fail to get response")
		}
	}

	if res.StatusCode != 200 {
		log.Fatal().Msg("Fail to register")
	}

	log.Info().Msg("Client registered")

	stateMap := map[sip.CallIDHeader]*Pixel{}
	coords_fmt := fmt.Sprintf("%s%%4d%%4d", *username)

	client.TransportLayer().OnMessage(func(msg sip.Message) {
		method := strings.SplitN(msg.String(), " ", 2)[0]

		switch method {
		case sip.INFO.String():
			// Signal= Split
			signalStr := string(msg.Body()[7:])
			signal, _ := strconv.Atoi(signalStr[:len(signalStr)-2])

			key, _ := msg.CallID()
			if val, ok := stateMap[*key]; ok {
				val.color = (val.color * 10) + signal
			}
		default:
			break
		}
	})

	for {
		phone := sipgox.NewPhone(ua)
		ctx, _ := context.WithCancel(context.Background())
		dialog, err := phone.Answer(ctx, sipgox.AnswerOptions{
			Ringtime: 0 * time.Second,
		})
		if err != nil {
			log.Fatal().Err(err).Msg("Fail to answer")
		}

		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt)

		callID, _ := dialog.InviteRequest.CallID()

		to, _ := dialog.InviteRequest.To()
		var x int
		var y int

		// TODO SLOW
		fmt.Sscanf(to.Address.User, coords_fmt, &x, &y)

		stateMap[*callID] = &Pixel{
			x,
			y,
			0,
		}

		select {
		case <-sig:
			ctx, _ := context.WithTimeout(context.Background(), 3*time.Second)
			dialog.Hangup(ctx)
			return

		case <-dialog.Done():
			fmt.Printf("DONE %+v\n", stateMap[*callID])

			con, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to connect to pixelflut server")
			}

			state := stateMap[*callID]
			r := (state.color / (1000 * 1000)) % 256
			g := ((state.color / 1000) % 1000) % 256
			b := ((state.color) % 1000) % 256

			con.Write([]byte(fmt.Sprintf("PX %d %d %02x%02x%02x\n", state.x, state.y, r, g, b)))
			con.Close()
		}
	}
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
