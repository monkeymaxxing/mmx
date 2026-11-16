package node

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"

	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// ErrUnknownRequest answers a name that is not one of protocol's JSON node
// requests.
var ErrUnknownRequest = errors.New("unknown node request")

type request func(s Server, ctx context.Context, body []byte, connection modules.ControllerConnection) (any, error)

// requests is the catalog of the JSON node requests that protocol names:
// what the node does for each. The node HTTPS API process and the node SSH
// request process both reach it through Answer.
var requests = map[string]request{
	protocol.RequestSubmit: func(s Server, _ context.Context, body []byte, connection modules.ControllerConnection) (any, error) {
		return s.Submit(body, connection)
	},
	protocol.RequestProgress: decoded(Server.Progress),
	protocol.RequestConfirmReachability: func(s Server, ctx context.Context, body []byte, connection modules.ControllerConnection) (any, error) {
		var request protocol.ReachabilityConfirmationRequest
		if err := protocol.Decode(body, &request); err != nil {
			return nil, err
		}
		return s.ConfirmReachability(ctx, request, connection.Kind)
	},
	protocol.RequestLastSuccessfulRun: decoded(func(s Server, request protocol.LastSuccessfulRunRequest) (any, error) {
		return map[string]string{"status": "recorded"}, s.RecordLastSuccessfulRun(request)
	}),
	protocol.RequestReadFile:      decoded(Server.ReadFile),
	protocol.RequestKeptFiles:     decoded(Server.UseKeptFiles),
	protocol.RequestFacts:         decoded(Server.Facts),
	protocol.RequestRebootRefusal: decoded(Server.RebootRefusal),
}

// RequestNames lists the JSON node requests in name order.
func RequestNames() []string {
	return slices.Sorted(maps.Keys(requests))
}

// decoded decodes the body into method's request type before calling method.
func decoded[Request, Answer any](method func(Server, Request) (Answer, error)) request {
	return func(s Server, _ context.Context, body []byte, _ modules.ControllerConnection) (any, error) {
		var request Request
		if err := protocol.Decode(body, &request); err != nil {
			return nil, err
		}
		return method(s, request)
	}
}

// Answer carries out the JSON node request called name. It reads the
// request from body, at most protocol.RequestLimit bytes whichever
// connection carries it, and returns the answer to send back as JSON.
// connection says which connection carried the request.
func (s Server) Answer(ctx context.Context, name string, body io.Reader, connection modules.ControllerConnection) (any, error) {
	answer, ok := requests[name]
	if !ok {
		return nil, ErrUnknownRequest
	}
	b, err := protocol.ReadLimited(body, protocol.RequestLimit, protocol.ErrRequestLimit)
	if err != nil {
		return nil, err
	}
	result, err := answer(s, ctx, b, connection)
	if err != nil {
		return nil, err
	}
	if response, ok := result.(protocol.Response); ok {
		return response.Transport(), nil
	}
	return result, nil
}
