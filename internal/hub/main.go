package hub

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

var commands = []models.BotCommand{
	{Command: "new_session", Description: "start a fresh Claude session"},
	{Command: "voice", Description: "show or set the TTS voice"},
	{Command: "status", Description: "hub, hosts and queue"},
	{Command: "target", Description: "show or set where messages go"},
}

// Main runs `hommy hub` for the instance cfg until ctx ends.
func Main(ctx context.Context, cfg *config.Config) error {
	if err := cfg.Export(); err != nil {
		return err
	}
	hc, err := LoadConfig(secret.Store{Service: cfg.Inst.KeychainService()})
	if err != nil {
		return err
	}
	st, err := OpenStore(hc.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	tg, err := newBotTelegram(hc.TelegramToken, hc.TelegramAPI)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	h := New(hc, st, tg, realSpeech{hc}, nil)
	if err := h.miniAppListen(ctx); err != nil {
		return fmt.Errorf("mini app: %w", err)
	}
	// A Bot API that can't be reached (or refuses the token) is reported by
	// /healthz and /api/status; the health loop keeps trying.
	h.refreshTelegram(ctx)
	var lns []net.Listener
	for _, addr := range listenAddrs(hc.Listen) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return err
		}
		lns = append(lns, ln)
	}
	paired := "unpaired"
	if h.ownerRow().Paired() {
		paired = "paired"
	}
	log.Printf("hommy hub %s up: host API on %s, hosts %v, default target %s, %s",
		version.String(), hc.Listen, h.hostNames(), hc.DefaultTarget, paired)
	go func() {
		if err := h.poll(ctx, hc.TelegramAPI, hc.TelegramToken, hc.AllowedUpdates); err != nil {
			log.Printf("telegram poller: %v", err)
		}
	}()
	return h.Run(ctx, lns[0], lns[1:]...)
}

// listenAddrs is HUB_LISTEN plus 127.0.0.1 on its port when HUB_LISTEN names one
// other address (a LAN IP for more Macs): this Mac's own host, pair, doctor and
// send dial HUB_URL, which stays on loopback.
func listenAddrs(listen string) []string {
	h, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{listen}
	}
	switch h {
	case "", "0.0.0.0", "::", "127.0.0.1", "localhost":
		return []string{listen}
	}
	return []string{listen, net.JoinHostPort("127.0.0.1", port)}
}
