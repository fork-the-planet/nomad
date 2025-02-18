# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: BUSL-1.1

locals {
  upload_dir          = "${var.uploads_dir}/${var.instance.public_ip}"
  shared_dir          = "${var.uploads_dir}/shared"
  indexed_config_path = fileexists("${path.module}/etc/nomad.d/${var.role}-${var.platform}-${var.index}.hcl") ? "${path.module}/etc/nomad.d/${var.role}-${var.platform}-${var.index}.hcl" : "${path.module}/etc/nomad.d/index.hcl"
}

# if nomad_license is unset, it'll be a harmless empty license file
resource "local_sensitive_file" "nomad_environment" {
  content = templatefile("${path.module}/etc/nomad.d/.environment", {
    license = var.nomad_license
  })
  filename        = "${local.upload_dir}/nomad.d/.environment"
  file_permission = "0600"
}

resource "local_sensitive_file" "nomad_base_config" {
  content = templatefile("${path.module}/etc/nomad.d/base.hcl", {
    data_dir     = var.platform != "windows" ? "/opt/nomad/data" : "C://opt/nomad/data"
    nomad_region = var.nomad_region
  })
  filename        = "${local.upload_dir}/nomad.d/base.hcl"
  file_permission = "0600"
}

resource "local_sensitive_file" "nomad_role_config" {
  content = templatefile("${path.module}/etc/nomad.d/${var.role}-${var.platform}.hcl", {
    aws_region     = var.aws_region
    aws_kms_key_id = var.aws_kms_key_id
  })
  filename        = "${local.upload_dir}/nomad.d/${var.role}.hcl"
  file_permission = "0600"
}

resource "local_sensitive_file" "nomad_indexed_config" {
  content         = templatefile(local.indexed_config_path, {})
  filename        = "${local.upload_dir}/nomad.d/${var.role}-${var.platform}-${var.index}.hcl"
  file_permission = "0600"
}

resource "local_sensitive_file" "nomad_tls_config" {
  content         = templatefile("${path.module}/etc/nomad.d/tls.hcl", {})
  filename        = "${local.upload_dir}/nomad.d/tls.hcl"
  file_permission = "0600"
}

resource "null_resource" "upload_consul_configs" {

  connection {
    type            = "ssh"
    user            = var.connection.user
    host            = var.instance.public_ip
    port            = var.connection.port
    private_key     = file(var.connection.private_key)
    target_platform = var.arch == "windows_amd64" ? "windows" : "unix"
    timeout         = "15m"
  }

  provisioner "file" {
    source      = "${local.shared_dir}/consul.d/agent_cert.key.pem"
    destination = "/tmp/consul_cert.key.pem"
  }
  provisioner "file" {
    source      = "${local.shared_dir}/consul.d/agent_cert.pem"
    destination = "/tmp/consul_cert.pem"
  }
  provisioner "file" {
    source      = "${var.keys_dir}/tls_ca.crt"
    destination = "/tmp/consul_ca.crt"
  }
  provisioner "file" {
    source      = "${local.shared_dir}/consul.d/clients.hcl"
    destination = "/tmp/consul_client.hcl"
  }
  provisioner "file" {
    source      = "${path.module}/etc/consul.d/consul.service"
    destination = "/tmp/consul.service"
  }
}

resource "null_resource" "upload_nomad_configs" {

  connection {
    type            = "ssh"
    user            = var.connection.user
    host            = var.instance.public_ip
    port            = var.connection.port
    private_key     = file(var.connection.private_key)
    target_platform = var.arch == "windows_amd64" ? "windows" : "unix"
    timeout         = "15m"
  }

  # created in consul-clients.tf
  provisioner "file" {
    source      = "${local.shared_dir}/nomad.d/${var.role}-consul.hcl"
    destination = "/tmp/consul.hcl"
  }
  # created in hcp_vault.tf
  provisioner "file" {
    source      = "${local.shared_dir}/nomad.d/vault.hcl"
    destination = "/tmp/vault.hcl"
  }

  provisioner "file" {
    source      = local_sensitive_file.nomad_environment.filename
    destination = "/tmp/.environment"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_base_config.filename
    destination = "/tmp/base.hcl"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_role_config.filename
    destination = "/tmp/${var.role}-${var.platform}.hcl"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_indexed_config.filename
    destination = "/tmp/${var.role}-${var.platform}-${var.index}.hcl"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_tls_config.filename
    destination = "/tmp/tls.hcl"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_systemd_unit_file.filename
    destination = "/tmp/nomad.service"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_client_key.filename
    destination = "/tmp/agent-${var.instance.public_ip}.key"
  }
  provisioner "file" {
    source      = local_sensitive_file.nomad_client_cert.filename
    destination = "/tmp/agent-${var.instance.public_ip}.crt"
  }
  provisioner "file" {
    source      = "${var.keys_dir}/tls_api_client.key"
    destination = "/tmp/tls_proxy.key"
  }
  provisioner "file" {
    source      = "${var.keys_dir}/tls_api_client.crt"
    destination = "/tmp/tls_proxy.crt"
  }
  provisioner "file" {
    source      = "${var.keys_dir}/tls_ca.crt"
    destination = "/tmp/ca.crt"
  }
  provisioner "file" {
    source      = "${var.keys_dir}/self_signed.key"
    destination = "/tmp/self_signed.key"
  }
  provisioner "file" {
    source      = "${var.keys_dir}/self_signed.crt"
    destination = "/tmp/self_signed.crt"
  }
}
