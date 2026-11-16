package node

import (
	"fmt"

	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// Facts answers the controller process when a fleet stage's when reads node
// facts: the controller process checks that condition just before the node
// would start. It measures the facts as a node job does, changes nothing,
// and takes no node lock, so a node with /etc/mmx/do_not_manage answers too.
func (s Server) Facts(request protocol.FactsRequest) (facts.Snapshot, error) {
	if !modules.Name.MatchString(request.Node) {
		return facts.Snapshot{}, fmt.Errorf("reading node facts requires the node's name from the inventory")
	}
	return facts.Collect(request.Node), nil
}
