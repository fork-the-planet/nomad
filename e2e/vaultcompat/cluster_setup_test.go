// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package vaultcompat

import "fmt"

const (
	// jwtPath is where the JWT auth method is mounted in Vault.
	// Use a non-default value for a more realistic scenario.
	jwtPath = "nomad_jwt"
)

// authConfigJWT is the configuration for the JWT auth method used by Nomad.
func authConfigJWT(jwksURL string) map[string]any {
	return map[string]any{
		"jwks_url":           jwksURL,
		"jwt_supported_algs": []string{"RS256", "EdDSA"},
		"default_role":       "nomad-workloads",
	}
}

// roleWID is the recommended role for Nomad workloads when using JWT and
// workload identity.
func roleWID(policies []string) map[string]any {
	return map[string]any{
		"role_type":               "jwt",
		"bound_audiences":         "vault.io",
		"user_claim":              "/extra_claims/nomad_workload_id",
		"user_claim_json_pointer": true,
		"claim_mappings": map[string]any{
			"nomad_namespace": "nomad_namespace",
			"nomad_job_id":    "nomad_job_id",
		},
		"token_type":     "service",
		"token_period":   "30m",
		"token_policies": policies,
	}
}

// policyWID is a templated Vault policy that grants tasks access to secret
// paths prefixed by <namespace>/<job>.
func policyWID(mountAccessor string) string {
	return fmt.Sprintf(`
path "secret/data/{{identity.entity.aliases.%[1]s.metadata.nomad_namespace}}/{{identity.entity.aliases.%[1]s.metadata.nomad_job_id}}/*" {
  capabilities = ["read"]
}

path "secret/data/{{identity.entity.aliases.%[1]s.metadata.nomad_namespace}}/{{identity.entity.aliases.%[1]s.metadata.nomad_job_id}}" {
  capabilities = ["read"]
}

path "secret/metadata/{{identity.entity.aliases.%[1]s.metadata.nomad_namespace}}/*" {
  capabilities = ["list"]
}

path "secret/metadata/*" {
  capabilities = ["list"]
}
`, mountAccessor)
}

// policyRestricted is Vault policy that only grants read access to a specific
// path.
const policyRestricted = `
path "secret/data/restricted" {
  capabilities = ["read"]
}

path "secret/metadata/restricted" {
  capabilities = ["list"]
}
`
