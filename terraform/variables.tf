variable "kubeconfig_path" {
  description = "Path to the kubeconfig k3d wrote."
  type        = string
  default     = "~/.kube/config"
}

variable "kube_context" {
  description = "Context to use. k3d names it k3d-<cluster>."
  type        = string
  default     = "k3d-webhook-relay"
}

# ---------------------------------------------------------------------------
# Profile
#
# One switch drives every memory-sensitive decision, so a low-memory run is a
# single variable rather than a dozen individually-remembered overrides.
# ---------------------------------------------------------------------------

variable "profile" {
  description = "Sizing profile: 'full' (~5.6 GiB) or 'lowmem' (~3.4 GiB)."
  type        = string
  default     = "full"

  validation {
    condition     = contains(["full", "lowmem"], var.profile)
    error_message = "profile must be either 'full' or 'lowmem'."
  }
}

# ---------------------------------------------------------------------------
# Namespaces
# ---------------------------------------------------------------------------

variable "namespaces" {
  description = "Namespaces to create, with the labels each needs."
  type = map(object({
    labels = map(string)
  }))
  default = {
    "webhook-relay" = {
      labels = {
        "app.kubernetes.io/part-of" = "webhook-relay"
        # NetworkPolicies select peer namespaces by label, not by name — a
        # namespaceSelector cannot match on metadata.name in most CNIs — so
        # every namespace the policies reference carries an explicit one.
        "name" = "webhook-relay"
      }
    }
    "observability" = {
      labels = {
        "app.kubernetes.io/part-of" = "webhook-relay"
        "name"                      = "observability"
      }
    }
    "argocd" = {
      labels = {
        "app.kubernetes.io/part-of" = "webhook-relay"
        "name"                      = "argocd"
      }
    }
    "chaos-testing" = {
      labels = {
        "app.kubernetes.io/part-of" = "webhook-relay"
        "name"                      = "chaos-testing"
        # Chaos Mesh injects a sidecar and needs elevated privileges to
        # manipulate network and process state on the node.
        "pod-security.kubernetes.io/enforce" = "privileged"
      }
    }
  }
}

# ---------------------------------------------------------------------------
# Postgres
# ---------------------------------------------------------------------------

variable "postgres_instances" {
  description = "CloudNativePG instance count. 3 gives automated failover; 1 does not."
  type        = number
  default     = 3

  validation {
    condition     = var.postgres_instances >= 1 && var.postgres_instances <= 5
    error_message = "postgres_instances must be between 1 and 5."
  }
}

variable "postgres_storage_size" {
  description = "PVC size per Postgres instance."
  type        = string
  default     = "2Gi"
}

variable "postgres_database" {
  description = "Application database name."
  type        = string
  default     = "webhook_relay"
}

variable "postgres_owner" {
  description = "Application database owner."
  type        = string
  default     = "webhook_relay"
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------

variable "enable_loki" {
  description = "Install Loki. Disabled under the lowmem profile."
  type        = bool
  default     = true
}

variable "enable_tempo" {
  description = "Install Tempo."
  type        = bool
  default     = true
}

variable "enable_chaos_mesh" {
  description = "Install Chaos Mesh. Used by Day 5."
  type        = bool
  default     = true
}

variable "prometheus_retention" {
  description = "How long Prometheus keeps samples."
  type        = string
  default     = "24h"
}

variable "prometheus_storage_size" {
  description = "Prometheus PVC size."
  type        = string
  default     = "4Gi"
}

variable "grafana_admin_password" {
  description = "Grafana admin password. Generated when empty."
  type        = string
  default     = ""
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Chart versions
#
# Pinned exactly, never a range. An unpinned chart means `terraform apply` on
# two different days produces two different clusters, which defeats the entire
# purpose of describing infrastructure as code.
# ---------------------------------------------------------------------------

variable "chart_versions" {
  description = "Pinned Helm chart versions."
  type        = map(string)
  default = {
    cloudnative_pg  = "0.23.0"
    kube_prometheus = "68.3.0"
    tempo           = "1.18.2"
    loki            = "6.24.0"
    ingress_nginx   = "4.12.0"
    argo_cd         = "7.7.16"
    chaos_mesh      = "2.7.0"
    sealed_secrets  = "2.16.2"
  }
}

# ---------------------------------------------------------------------------
# GitOps
# ---------------------------------------------------------------------------

variable "gitops_repo_url" {
  description = "Repository ArgoCD reconciles from. Public, so no credential is needed."
  type        = string
  default     = "https://github.com/singha105/webhook-relay.git"
}

variable "gitops_target_revision" {
  description = "Branch or tag ArgoCD tracks."
  type        = string
  default     = "main"
}

variable "sealed_secrets_version" {
  description = "Sealed Secrets release to install, without the leading v."
  type        = string
  default     = "0.27.3"
}
