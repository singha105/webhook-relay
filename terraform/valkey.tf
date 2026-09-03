# Valkey: the work queue, and the store for rate-limit buckets, breaker state,
# and delivery dedup keys.
#
# WHY THIS IS PLAIN KUBERNETES RESOURCES AND NOT A HELM CHART
#
# The Bitnami chart was the obvious choice and it does not work. Bitnami's 2025
# licensing change removed their public chart index (their sealed-secrets and
# charts.bitnami.com repositories now 404 or redirect) and moved their images
# behind a subscription — `docker pull bitnami/valkey:8.0.2-debian-12-r0`
# returns "not found". A chart whose images cannot be pulled is worse than no
# chart, because the failure appears at pod-scheduling time as ImagePullBackOff
# rather than at apply time.
#
# What Valkey needs here is one Deployment, one Service, and a config file.
# That is less code than the chart's values block was, has no external
# dependency that can disappear, and uses the SAME official image the
# docker-compose stack has been running since Day 2 — so behaviour is
# identical between local and cluster.
#
# Single replica, no persistence, deliberately. The durable record of every
# event is in Postgres; the stream is a transport for work that is ready now.
# Losing Valkey costs in-flight queue entries, which the outbox relay
# re-enqueues from Postgres, and cached breaker state, which rebuilds from the
# next failure. Day 5 deletes this pod on purpose to prove exactly that.

locals {
  valkey_labels = merge(local.common_labels, {
    "app.kubernetes.io/name"      = "valkey"
    "app.kubernetes.io/component" = "queue"
  })
}

resource "kubernetes_config_map" "valkey" {
  metadata {
    name      = "valkey-config"
    namespace = kubernetes_namespace.this["webhook-relay"].metadata[0].name
    labels    = local.valkey_labels
  }

  data = {
    "valkey.conf" = <<-EOT
      bind 0.0.0.0
      port 6379
      protected-mode no

      maxmemory 200mb
      # noeviction: this is a QUEUE, not a cache. Silently evicting a stream
      # entry under memory pressure would lose work with no error anywhere —
      # far worse than refusing the write, which the relay retries.
      maxmemory-policy noeviction

      # No persistence. See the file header: durability lives in Postgres, and
      # an AOF here would hide the failure mode Day 5 exists to exercise.
      appendonly no
      save ""

      loglevel notice
    EOT
  }
}

resource "kubernetes_deployment" "valkey" {
  metadata {
    name      = "valkey"
    namespace = kubernetes_namespace.this["webhook-relay"].metadata[0].name
    labels    = local.valkey_labels
  }

  spec {
    replicas = 1

    # Recreate, not RollingUpdate. Two Valkey pods briefly serving the same
    # Service would split the stream: some workers would read a consumer group
    # that the others cannot see, and entries claimed against one pod would
    # never be acked on the other.
    strategy {
      type = "Recreate"
    }

    selector {
      match_labels = {
        "app.kubernetes.io/name" = "valkey"
      }
    }

    template {
      metadata {
        labels = local.valkey_labels
      }

      spec {
        container {
          name = "valkey"
          # Pinned by DIGEST, not by tag. "8-alpine" is mutable — the image it
          # resolves to today is not the one it resolves to next month, which
          # makes a cluster rebuild non-reproducible and a rollback
          # meaningless. This is the same argument the Helm chart makes for
          # refusing a mutable application tag.
          image = "valkey/valkey@sha256:b21fd94099dcd4bc6b2b9230daef69b6558b887ad4a2a1afe56ff6e745a88cdb"
          args  = ["/etc/valkey/valkey.conf"]

          port {
            name           = "valkey"
            container_port = 6379
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/valkey"
          }
          volume_mount {
            name       = "data"
            mount_path = "/data"
          }

          resources {
            requests = { cpu = "50m", memory = "128Mi" }
            # The limit sits above the configured maxmemory of 200mb so Valkey
            # refuses a write before the kernel OOM-kills the pod. A limit at
            # or below maxmemory would turn a full queue into a crash loop.
            limits = { memory = "320Mi" }
          }

          liveness_probe {
            tcp_socket { port = "valkey" }
            initial_delay_seconds = 5
            period_seconds        = 10
          }

          readiness_probe {
            exec {
              command = ["valkey-cli", "ping"]
            }
            initial_delay_seconds = 2
            period_seconds        = 5
          }

          security_context {
            allow_privilege_escalation = false
            run_as_non_root            = true
            run_as_user                = 999
            # Valkey writes only to /data, which is an emptyDir mounted below.
            # Persistence is off, so nothing else on disk is ever written.
            read_only_root_filesystem = true
            capabilities { drop = ["ALL"] }
          }
        }

        volume {
          name = "config"
          config_map { name = kubernetes_config_map.valkey.metadata[0].name }
        }
        volume {
          name = "data"
          empty_dir {}
        }
      }
    }
  }
}

resource "kubernetes_service" "valkey" {
  metadata {
    # Named valkey-master to match the hostname the Helm chart would have
    # produced, so the application's default config is unchanged between the
    # compose stack and the cluster.
    name      = "valkey-master"
    namespace = kubernetes_namespace.this["webhook-relay"].metadata[0].name
    labels    = local.valkey_labels
  }

  spec {
    selector = {
      "app.kubernetes.io/name" = "valkey"
    }
    port {
      name        = "valkey"
      port        = 6379
      target_port = "valkey"
    }
  }
}
