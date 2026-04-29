package common

// WebhookPath is the single mount point both zalo_bot and zalo_oa channel
// instances dispatch through. The per-instance routing is keyed off the
// `?instance=<uuid>` query param inside the shared Router.
const WebhookPath = "/channels/zalo/webhook"

// sharedRouter is the process-global router both zalo_bot and zalo_oa
// channels register into. Constructed at package init so MountRoute() is
// safe to call from any goroutine without lazy-init races. Mirrors
// facebook/webhook_router.go and pancake/webhook_handler.go.
var sharedRouter = NewRouter()

// SharedRouter returns the process-global router. Production code path
// only — tests construct isolated routers via NewRouter() and assign
// directly to the channel field (white-box, same-package access).
func SharedRouter() *Router { return sharedRouter }
