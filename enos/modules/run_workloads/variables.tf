# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: BUSL-1.1

variable "nomad_addr" {
  description = "The Nomad API HTTP address."
  type        = string
  default     = "http://localhost:4646"
}

variable "ca_file" {
  description = "A local file path to a PEM-encoded certificate authority used to verify the remote agent's certificate"
  type        = string
}

variable "cert_file" {
  description = "A local file path to a PEM-encoded certificate provided to the remote agent. If this is specified, key_file or key_pem is also required"
  type        = string
}

variable "key_file" {
  description = "A local file path to a PEM-encoded private key. This is required if cert_file or cert_pem is specified."
  type        = string
}

variable "nomad_token" {
  description = "The Secret ID of an ACL token to make requests with, for ACL-enabled clusters."
  type        = string
  sensitive   = true
}

variable "consul_addr" {
  description = "The Consul API HTTP address."
  type        = string
  default     = "http://localhost:8500"
}

variable "consul_token" {
  description = "The Secret ID of an ACL token to make requests to Consul with"
  type        = string
  sensitive   = true
}

variable "availability_zone" {
  description = "The AZ where the cluster is being run"
  type        = string
}

variable "vault_addr" {
  description = "The Vault API HTTP address."
  type        = string
  default     = "http://localhost:8200"
}

variable "vault_token" {
  description = "The Secret ID of an ACL token to make requests to Vault with"
  type        = string
  sensitive   = true
}

variable "vault_mount_path" {
  description = "The path where the provision_cluster modules enables a secrets engine "
  type        = string
  default     = "admin"
}

variable "workloads" {
  description = "A map of workloads to provision"

  type = map(object({
    job_spec    = string
    alloc_count = number
    type        = string
    pre_script  = optional(string)
    post_script = optional(string)
  }))

  validation {
    condition = alltrue([
      for w in values(var.workloads) : contains(["service", "batch", "system"], w.type)
    ])
    error_message = "Each workload must have a 'type' value of either service, batch or system"
  }
}
