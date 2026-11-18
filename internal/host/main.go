package host

import (
	"context"
	"log"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

// Main runs `hommy host` until ctx ends.
func Main(ctx context.Context, ic *config.Config) error {
	if err := ic.Export(); err != nil {
		return err
	}
	store := secret.Store{Service: ic.Inst.KeychainService()}
	cfg, err := LoadConfig(func() (string, error) { return store.Get(secret.HostKey) })
	if err != nil {
		return err
	}
	h, err := New(cfg)
	if err != nil {
		return err
	}
	defer h.Close()
	h.SetChecks(checksFunc(cfg))
	log.Printf("agent command: %s", cfg.AgentCommandLine())
	if ctl, err := agterm.NewExecCtl(); err != nil {
		log.Printf("agterm jobs off: %v", err)
	} else {
		RegisterAgterm(ctx, h, ctl)
	}
	if _, err := h.StartHookRelay(ctx); err != nil {
		log.Printf("hookrelay off: %v", err) // answers from interactive sessions don't come back
	}
	log.Printf("hommy host %s %s up: hub %s, vault %s, data %s, workspace %s", cfg.Name, version.String(), cfg.HubURL,
		cfg.VaultDir, cfg.DataDir, cfg.Workspace)
	return h.Run(ctx)
}
