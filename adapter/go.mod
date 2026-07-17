module github.com/daboluocc/bbclaw/adapter

go 1.24

require (
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/spf13/cobra v1.10.2
	github.com/sst/opencode-sdk-go v0.19.2
	github.com/zhoushoujianwork/agent-runner v0.5.1
)

require (
	github.com/daboluocc/bbclaw/voice v0.0.0
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/tidwall/gjson v1.14.4 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
)

replace github.com/daboluocc/bbclaw/voice => ../voice
