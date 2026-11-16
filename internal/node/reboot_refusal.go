package node

import "mmx/internal/protocol"

// RebootRefusal answers the controller process before the node job of a
// fleet stage that ends with a reboot task: the message with which this node
// refuses the reboot because it is the machine that runs the controller
// process, or nothing. The rule is engine.NodeMachine.RebootRefusal, which
// the reboot task applies again. The answer changes nothing and takes no
// node lock.
func (s Server) RebootRefusal(request protocol.RebootRefusalRequest) (protocol.RebootRefusalResponse, error) {
	refusal, err := s.machine.RebootRefusal(request.ControllerMachine)
	return protocol.RebootRefusalResponse{Refusal: refusal}, err
}
