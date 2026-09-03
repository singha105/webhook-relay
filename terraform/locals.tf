locals {
  lowmem = var.profile == "lowmem"

  # Every memory-sensitive decision derived from the one profile switch. A
  # reviewer can see the whole cost of "lowmem" in one place instead of hunting
  # for conditionals across six files.
  effective = {
    postgres_instances      = local.lowmem ? 1 : var.postgres_instances
    enable_loki             = local.lowmem ? false : var.enable_loki
    enable_tempo            = var.enable_tempo
    enable_chaos_mesh       = local.lowmem ? false : var.enable_chaos_mesh
    prometheus_retention    = local.lowmem ? "6h" : var.prometheus_retention
    prometheus_storage_size = local.lowmem ? "2Gi" : var.prometheus_storage_size
    prometheus_memory       = local.lowmem ? "512Mi" : "1Gi"
    postgres_memory         = local.lowmem ? "256Mi" : "384Mi"
  }

  common_labels = {
    "app.kubernetes.io/part-of"    = "webhook-relay"
    "app.kubernetes.io/managed-by" = "terraform"
  }
}
