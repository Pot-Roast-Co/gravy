package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pot-roast-co/gravy/internal/notify"
)

func runNotifyTest(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: gravy notify-test [ticket-id]")
	}
	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()
	notifier := notify.New(a.cfg.Notifications, os.Stderr, notify.WithRunner(hostRunner{h: a.host}))
	id := ""
	if len(args) == 1 {
		id = args[0]
	}
	notifier.NotifyTicket(ctx, "Gravy · notification preview", "A quiet chime. Click to open Gravy.", notify.Normal, id)
	return nil
}
