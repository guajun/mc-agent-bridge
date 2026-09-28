package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/guajun/mc-agent-bridge/internal/daemon"
	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

// runDaemon starts the blocking daemon loop for `mc-agent daemon run`.
func (a *app) runDaemon(options daemonOptions) *protocol.Error {
	err := daemon.Run(context.Background(), daemon.Options{
		Home:           a.home,
		APIAddress:     options.apiAddress,
		BufferSize:     options.buffer,
		ReconnectDelay: options.reconnectDelay,
		Webhook:        options.webhook,
		Fake:           options.fake,
		Logger: func(format string, args ...any) {
			fmt.Fprintf(a.stderr, "[mc-agent] "+format+"\n", args...)
		},
	})
	if err != nil {
		var protocolErr *protocol.Error
		if ok := asProtocolError(err, &protocolErr); ok {
			return protocolErr
		}
		return protocol.NewError(protocol.CodeInternal, err.Error())
	}
	return nil
}

func asProtocolError(err error, target **protocol.Error) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(*protocol.Error); ok {
		*target = typed
		return true
	}
	return false
}

var _ = os.Getenv
