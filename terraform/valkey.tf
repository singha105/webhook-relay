# Valkey: the work queue and the store for rate-limit buckets, breaker state,
# and delivery dedup keys.
#
# Single replica, no persistence. That is a deliberate statement about what
# Valkey is for in this system: the durable record of every event lives in
# Postgres, and the stream is a transport for work that is ready now. Losing
# Valkey costs in-flight queue entries, which the outbox relay re-enqueues from
# Postgres, and cached breaker state, which rebuilds from the next failure.
#
# Day 5 deletes this pod on purpose to prove exactly that.

resource "helm_release" "valkey" {
  name       = "valkey"
  repository = "https://charts.bitnami.com/bitnami"
  chart      = "valkey"
  version    = var.chart_versions.valkey
  namespace  = kubernetes_namespace.this["webhook-relay"].metadata[0].name

  atomic          = true
  cleanup_on_fail = true
  timeout         = 600

  values = [yamlencode({
    architecture = "standalone"

    auth = {
      # No password. The NetworkPolicy in the Helm chart restricts ingress to
      # the api and worker pods, so the network is the boundary rather than a
      # credential that would have to be rotated and distributed. A real
      # deployment crossing a trust boundary would enable auth; here the
      # boundary is inside one namespace on one node.
      enabled = false
    }

    master = {
      persistence = {
        # Off on purpose. An AOF or RDB file would make restarts preserve queue
        # state, which sounds better but hides the failure mode Day 5 exists to
        # exercise: the system must survive Valkey coming back empty.
        enabled = false
      }
      resources = {
        requests = { cpu = "50m", memory = "128Mi" }
        limits   = { memory = "256Mi" }
      }
      # maxmemory-policy noeviction: this is a queue, not a cache. Silently
      # evicting a stream entry under memory pressure would lose work with no
      # error anywhere — far worse than refusing the write, which the relay
      # retries.
      configuration = <<-EOT
        maxmemory 200mb
        maxmemory-policy noeviction
        appendonly no
        save ""
      EOT
    }

    metrics = {
      enabled = true
      serviceMonitor = {
        enabled   = true
        namespace = kubernetes_namespace.this["observability"].metadata[0].name
      }
      resources = {
        requests = { cpu = "10m", memory = "32Mi" }
        limits   = { memory = "64Mi" }
      }
    }

    replica = {
      replicaCount = 0
    }
  })]
}
