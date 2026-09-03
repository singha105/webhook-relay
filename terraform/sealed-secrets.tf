# Sealed Secrets.
#
# WHY THIS AND NOT SOPS
#
# Both solve "how do you put secrets in Git". The deciding factor here is
# ArgoCD.
#
#   Sealed Secrets: a SealedSecret is a normal custom resource. ArgoCD applies
#   it like any other manifest and the controller decrypts it into a Secret
#   in-cluster. Nothing about the delivery pipeline changes.
#
#   SOPS: the ciphertext is inside an otherwise-normal Secret manifest, so
#   something has to decrypt it BEFORE apply. With ArgoCD that means a custom
#   config-management plugin (ksops, argocd-vault-plugin), which means a
#   sidecar on the repo-server, a private key mounted into it, and a second
#   thing that can break between a commit and a deployment.
#
# SOPS is the better answer when the secret must be readable outside a cluster
# — a Terraform variable, a local .env, a CI job — because a SealedSecret can
# only be decrypted by the controller that holds the key. This project has both
# needs, so both are set up: Sealed Secrets for anything ArgoCD deploys, SOPS
# for the handful of values a human or a CI job needs. See docs/secrets.md.
#
# THE KEY
#
# The controller generates an RSA keypair on first start and stores it as a
# Secret in its own namespace. The PUBLIC half is what kubeseal fetches to
# encrypt; the private half never leaves the cluster and is never committed.
#
# That has a consequence people discover the hard way: recreate the cluster and
# every committed SealedSecret becomes undecryptable, because the new
# controller has a new key. `make seal-backup` exports the key so a rebuilt
# cluster can decrypt the existing ciphertext. Losing it means re-sealing every
# secret, which is survivable here and career-limiting in production.

# The Helm repository no longer resolves — Bitnami's 2025 licensing change took
# https://bitnami-labs.github.io/sealed-secrets with it (404). The upstream
# project still publishes a plain manifest per release, which is a more stable
# artifact anyway: it is a single URL pinned to a tag, with no index to go
# missing.
data "http" "sealed_secrets_manifest" {
  url = "https://github.com/bitnami-labs/sealed-secrets/releases/download/v${var.sealed_secrets_version}/controller.yaml"

  lifecycle {
    postcondition {
      condition     = self.status_code == 200
      error_message = "Could not download the Sealed Secrets manifest (HTTP ${self.status_code})."
    }
  }
}

# The manifest is multi-document; split it so each object is a separate
# resource Terraform can track and destroy individually.
data "kubectl_file_documents" "sealed_secrets" {
  content = data.http.sealed_secrets_manifest.response_body
}

resource "kubectl_manifest" "sealed_secrets" {
  for_each = data.kubectl_file_documents.sealed_secrets.manifests

  yaml_body = each.value

  # CRDs must exist before the CustomResource-consuming objects that follow.
  # server_side_apply avoids the "metadata.annotations: Too long" failure that
  # client-side apply hits on large CRDs.
  server_side_apply = true
  force_conflicts   = true

  depends_on = [kubernetes_namespace.this]
}
