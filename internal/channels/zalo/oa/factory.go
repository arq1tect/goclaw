package oa

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/zalo/common"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Factory returns a channels.ChannelFactory closure that captures the
// store dependency. Kept for back-compat with call sites that don't yet
// thread the shared webhook router; new code should prefer FactoryWithRouter.
func Factory(ciStore store.ChannelInstanceStore) channels.ChannelFactory {
	return factoryWith(ciStore, nil)
}

// FactoryWithRouter is the preferred factory: it threads the shared
// webhook router into the channel so phases 05+ can register/unregister
// per-instance webhook handlers at Start()/Stop().
func FactoryWithRouter(ciStore store.ChannelInstanceStore, router *common.Router) channels.ChannelFactory {
	return factoryWith(ciStore, router)
}

func factoryWith(ciStore store.ChannelInstanceStore, router *common.Router) channels.ChannelFactory {
	return func(name string, credsRaw json.RawMessage, cfgRaw json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

		if ciStore == nil {
			return nil, errors.New("zalo_oa: nil ChannelInstanceStore")
		}

		creds, err := LoadCreds(credsRaw)
		if err != nil {
			return nil, fmt.Errorf("zalo_oa: decode credentials: %w", err)
		}

		var cfg config.ZaloOAConfig
		if len(cfgRaw) > 0 {
			if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
				return nil, fmt.Errorf("zalo_oa: decode config: %w", err)
			}
		}

		ch, err := New(name, cfg, creds, ciStore, msgBus, pairingSvc)
		if err != nil {
			return nil, err
		}
		ch.webhookRouter = router
		// Seed the in-memory poll cursor from any persisted state in
		// channel_instances.config.poll_cursor (phase-04 persistence).
		if seeded := parseCursorFromConfig(cfgRaw); len(seeded) > 0 {
			ch.cursor.loadFromMap(seeded)
		}
		return ch, nil
	}
}
