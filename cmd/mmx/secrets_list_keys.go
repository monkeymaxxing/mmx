package main

import (
	"context"
	"fmt"

	"mmx/internal/secrets"
)

// secretsListKeys prints the vault's top-level keys, sorted, one on each
// line, and never a value. It only reads the vault.
func secretsListKeys(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) != 0 {
		return 4, fmt.Errorf("mmx secrets list-keys takes no arguments")
	}
	vault, _, values, err := o.existingVault(ctx)
	if err != nil {
		return secretsFailure(err)
	}
	keys := secrets.Keys(values)
	if o.asJSON {
		return encode(struct {
			SchemaVersion int      `json:"schema_version"`
			Vault         string   `json:"vault"`
			Keys          []string `json:"keys"`
		}{1, vault, keys})
	}
	for _, key := range keys {
		fmt.Println(key)
	}
	return 0, nil
}
