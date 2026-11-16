package node

import "fmt"

func requireMemoryFilesystem(string) error { return fmt.Errorf("node jobs require Linux") }
