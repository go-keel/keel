// Command knats-deadletter lists the messages knats consumers gave up on and replays them.
//
//	go run github.com/go-keel/keel/core/knats/cmd/knats-deadletter -url nats://host:4222 list [-consumer NAME]
//	go run github.com/go-keel/keel/core/knats/cmd/knats-deadletter -url nats://host:4222 replay SEQ...
//
// Replay republishes the original message to its subject, so every consumer of that subject receives it again, not
// only the one that gave up. Fix the cause first.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/go-keel/keel/core/knats"
)

func main() {
	url := flag.String("url", nats.DefaultURL, "NATS server URL")
	creds := flag.String("creds", "", "NATS credentials file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: knats-deadletter [-url URL] [-creds FILE] list [-consumer NAME] | replay SEQ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := run(*url, *creds, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "knats-deadletter:", err)
		os.Exit(1)
	}
}

func run(url, creds string, args []string) error {
	if len(args) == 0 {
		flag.Usage()
		return fmt.Errorf("missing command")
	}
	var opts []nats.Option
	if creds != "" {
		opts = append(opts, nats.UserCredentials(creds))
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		consumer := fs.String("consumer", "", "only this durable")
		_ = fs.Parse(args[1:])
		dls, err := knats.ListDeadLetters(ctx, js)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		// tabwriter buffers; write errors surface in Flush below.
		_, _ = fmt.Fprintln(w, "SEQ\tTIME\tSTREAM\tSTREAM_SEQ\tCONSUMER\tDELIVERIES\tREASON")
		for _, d := range dls {
			if *consumer != "" && d.Consumer != *consumer {
				continue
			}
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%d\t%s\n", d.Seq, d.Time.UTC().Format(time.RFC3339), d.Stream, d.StreamSeq,
				d.Consumer, d.Deliveries, d.Reason)
		}
		return w.Flush()
	case "replay":
		if len(args) < 2 {
			return fmt.Errorf("replay needs at least one SEQ from list")
		}
		for _, a := range args[1:] {
			seq, err := strconv.ParseUint(a, 10, 64)
			if err != nil {
				return fmt.Errorf("SEQ %q is not a number", a)
			}
			if err := knats.ReplayDeadLetter(ctx, js, seq); err != nil {
				return fmt.Errorf("replay %d: %w", seq, err)
			}
			fmt.Printf("replayed %d\n", seq)
		}
		return nil
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}
