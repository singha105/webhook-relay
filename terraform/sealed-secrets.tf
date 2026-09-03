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

resource "helm_release" "sealed_secrets" {
  name       = "sealed-secrets"
  repository = "https://bitnami-labs.github.io/sealed-secrets"
  chart      = "sealed-secrets"
  version    = var.chart_versions.sealed_secrets
  namespace  = "kube-system"

  atomic          = true
  cleanup_on_fail = true
  timeout         = 600

  values = [yamlencode({
    fullnameOverride = "sealed-secrets-controller"
    resources = {
      requests = { cpu = "25m", memory = "64Mi" }
      limits   = { memory = "128Mi" }
    }
    metrics = {
      serviceMonitor = {
        enabled   = true
        namespace = kubernetes_namespace.this["observability"].metadata[0].name
      }
    }
  })]

  depends_on = [kubernetes_namespace.this]
}
