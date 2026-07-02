module github.com/shindeamul76/optedge-live

go 1.25.1

require (
	github.com/gorilla/websocket v1.5.3
	github.com/shindeamul76/optedge v1.2.0
	gopkg.in/yaml.v3 v3.0.1
)

// Dev-only: build against the local frozen engine. REMOVE before freeze and set
// GOPRIVATE=github.com/shindeamul76/* so the real tagged module is fetched.
// NOTE: the engine repo folder was renamed intraday-trading-go -> optedge.
replace github.com/shindeamul76/optedge => ../optedge
