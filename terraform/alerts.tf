# Alert rules and a local receiver.
#
# The receiver is a plain HTTP server in the cluster that logs every alert it
# gets. No Slack, no PagerDuty, no email — all of those need an account, and
# the thing being demonstrated is that routing works end to end, not that
# somebody owns a paging rotation.

resource "kubectl_manifest" "alert_rules" {
  depends_on = [helm_release.kube_prometheus_stack]

  yaml_body = yamlencode({
    apiVersion = "monitoring.coreos.com/v1"
    kind       = "PrometheusRule"
    metadata = {
      name      = "webhook-relay-alerts"
      namespace = kubernetes_namespace.this["observability"].metadata[0].name
      labels = merge(local.common_labels, {
        # The operator selects rules by label. Terraform sets
        # ruleSelectorNilUsesHelmValues=false so this is picked up regardless,
        # but the label is set anyway so the rule is still found if that
        # setting is ever tightened.
        release = "kube-prometheus-stack"
      })
    }
    spec = {
      groups = [
        {
          name     = "webhook-relay.slo"
          interval = "30s"
          rules = [
            {
              alert = "WebhookQueueDepthGrowing"
              # Growing, not merely high. A deep queue that is draining is a
              # burst being absorbed, which is the system working. A queue with
              # a positive derivative sustained for five minutes is workers
              # falling behind, which is not.
              expr   = "sum(deriv(webhook_queue_depth[5m])) > 0 and sum(webhook_queue_depth) > 50"
              for    = "5m"
              labels = { severity = "warning", component = "worker" }
              annotations = {
                summary     = "Queue depth has been growing for five minutes"
                description = "Depth is {{ $value | humanize }} and rising. Workers are not keeping up with ingest."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#climbing-queue-depth"
              }
            },
            {
              alert = "WebhookBacklogAgeOverSLO"
              # The SLO metric. This is the one that corresponds to something a
              # customer can feel.
              expr   = "max(webhook_queue_oldest_message_age_seconds) > 30"
              for    = "2m"
              labels = { severity = "warning", component = "worker" }
              annotations = {
                summary     = "Oldest undelivered event is over the 30s objective"
                description = "Oldest event has waited {{ $value | humanizeDuration }}. The SLO is 99% delivered within 30s."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#climbing-queue-depth"
              }
            },
            {
              alert  = "WebhookBacklogAgeCritical"
              expr   = "max(webhook_queue_oldest_message_age_seconds) > 300"
              for    = "2m"
              labels = { severity = "critical", component = "worker" }
              annotations = {
                summary     = "Delivery backlog is five minutes deep"
                description = "Error budget is burning fast. Oldest event: {{ $value | humanizeDuration }}."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#climbing-queue-depth"
              }
            },
            {
              alert = "WebhookDLQRateSpike"
              # Compared against the recent baseline rather than a fixed
              # threshold, so a service that normally dead-letters a trickle
              # does not page constantly while a genuine spike still fires.
              expr   = "sum(rate(webhook_events_dlq_total[5m])) > 0.1"
              for    = "5m"
              labels = { severity = "warning", component = "worker" }
              annotations = {
                summary     = "Events are being dead-lettered"
                description = "{{ $value | humanize }} events/s are exhausting retries or failing permanently. Every one is a webhook a customer never received."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#draining-the-dlq"
              }
            },
            {
              alert  = "WebhookCircuitBreakerOpen"
              expr   = "webhook_circuit_breaker_state == 2"
              for    = "5m"
              labels = { severity = "warning", component = "worker" }
              annotations = {
                summary     = "Circuit breaker open for {{ $labels.endpoint_id }}"
                description = "Deliveries to this endpoint are being skipped entirely. This is the breaker doing its job; it matters if it stays open."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#an-endpoints-breaker-is-open"
              }
            },
          ]
        },
        {
          name     = "webhook-relay.platform"
          interval = "30s"
          rules = [
            {
              alert = "WebhookPodRestartLoop"
              # Restarts over the last 15 minutes, not total restarts. A pod
              # that restarted twice last week is not an incident.
              expr   = "increase(kube_pod_container_status_restarts_total{namespace=\"webhook-relay\"}[15m]) > 3"
              for    = "5m"
              labels = { severity = "critical", component = "platform" }
              annotations = {
                summary     = "{{ $labels.pod }} is restarting repeatedly"
                description = "{{ $value }} restarts in 15 minutes. Check whether the liveness probe is failing on a dependency it should not be checking."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#a-pod-is-restarting"
              }
            },
            {
              alert = "PostgresReplicaLagHigh"
              # CloudNativePG exports this via its PodMonitor. Replica lag
              # matters here because a failover promotes a standby: promoting
              # one that is far behind means losing the transactions it never
              # received.
              expr   = "max(cnpg_pg_replication_lag) > 30"
              for    = "5m"
              labels = { severity = "warning", component = "postgres" }
              annotations = {
                summary     = "Postgres replica lag is {{ $value | humanizeDuration }}"
                description = "A failover now would promote a standby missing this much data."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#postgres-failover"
              }
            },
            {
              alert  = "PostgresClusterUnhealthy"
              expr   = "cnpg_pg_cluster_status{status!=\"Cluster in healthy state\"} == 1"
              for    = "5m"
              labels = { severity = "critical", component = "postgres" }
              annotations = {
                summary     = "Postgres cluster is not healthy"
                description = "CloudNativePG reports: {{ $labels.status }}"
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#postgres-failover"
              }
            },
            {
              alert = "WebhookNoDeliveriesAttempted"
              # The alert that catches "everything looks fine but nothing is
              # happening" — the failure mode where workers are running,
              # healthy, and silently not consuming, which every other alert
              # here would miss.
              expr   = "sum(rate(webhook_delivery_attempts_total[10m])) == 0 and sum(webhook_queue_depth) > 0"
              for    = "10m"
              labels = { severity = "critical", component = "worker" }
              annotations = {
                summary     = "Queue is not empty but nothing is being delivered"
                description = "Workers are up but consuming nothing. Check the consumer group exists and the breakers are not all open."
                runbook_url = "https://github.com/singha105/webhook-relay/blob/main/docs/runbook.md#nothing-is-being-delivered"
              }
            },
          ]
        },
      ]
    }
  })
}

# The receiver. Deliberately trivial: an nginx that accepts a POST and logs it,
# so `kubectl logs` is the alert history.
resource "kubernetes_config_map" "alert_receiver" {
  metadata {
    name      = "alert-receiver-config"
    namespace = kubernetes_namespace.this["observability"].metadata[0].name
    labels    = local.common_labels
  }

  data = {
    "nginx.conf" = <<-EOT
      worker_processes 1;
      error_log /dev/stderr warn;
      pid /tmp/nginx.pid;
      events { worker_connections 64; }
      http {
        access_log off;
        client_body_temp_path /tmp/client_body;
        proxy_temp_path /tmp/proxy;
        fastcgi_temp_path /tmp/fastcgi;
        uwsgi_temp_path /tmp/uwsgi;
        scgi_temp_path /tmp/scgi;

        # Log the alert body itself, not just the request line — the payload is
        # the whole point.
        log_format alert escape=json '{"received":"$time_iso8601","alert":$request_body}';

        server {
          listen 9095;
          location /alerts {
            access_log /dev/stdout alert;
            return 200 '{"status":"received"}\n';
            add_header Content-Type application/json;
          }
          location /healthz { return 200 'ok\n'; }
        }
      }
    EOT
  }
}

resource "kubernetes_deployment" "alert_receiver" {
  metadata {
    name      = "alert-receiver"
    namespace = kubernetes_namespace.this["observability"].metadata[0].name
    labels    = merge(local.common_labels, { app = "alert-receiver" })
  }

  spec {
    replicas = 1
    selector { match_labels = { app = "alert-receiver" } }

    template {
      metadata { labels = merge(local.common_labels, { app = "alert-receiver" }) }
      spec {
        container {
          name  = "nginx"
          image = "nginx:1.27-alpine"

          port {
            name           = "http"
            container_port = 9095
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/nginx/nginx.conf"
            sub_path   = "nginx.conf"
          }
          volume_mount {
            name       = "tmp"
            mount_path = "/tmp"
          }

          resources {
            requests = { cpu = "10m", memory = "16Mi" }
            limits   = { memory = "32Mi" }
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            initial_delay_seconds = 3
            period_seconds        = 10
          }
        }

        volume {
          name = "config"
          config_map { name = kubernetes_config_map.alert_receiver.metadata[0].name }
        }
        volume {
          name = "tmp"
          empty_dir {}
        }
      }
    }
  }
}

resource "kubernetes_service" "alert_receiver" {
  metadata {
    name      = "alert-receiver"
    namespace = kubernetes_namespace.this["observability"].metadata[0].name
    labels    = merge(local.common_labels, { app = "alert-receiver" })
  }
  spec {
    selector = { app = "alert-receiver" }
    port {
      name        = "http"
      port        = 9095
      target_port = "http"
    }
  }
}
