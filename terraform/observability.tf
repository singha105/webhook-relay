# kube-prometheus-stack, Tempo and Loki.
#
# This replaces the docker-compose observability stack from Day 3. The metric
# names, the dashboard JSON, the recording rules and the SLO queries are all
# unchanged — that is the point of having pinned the metric names with a test.

resource "random_password" "grafana_admin" {
  length  = 24
  special = false
}

locals {
  grafana_password = var.grafana_admin_password != "" ? var.grafana_admin_password : random_password.grafana_admin.result
}

resource "helm_release" "kube_prometheus_stack" {
  name       = "kube-prometheus-stack"
  repository = "https://prometheus-community.github.io/helm-charts"
  chart      = "kube-prometheus-stack"
  version    = var.chart_versions.kube_prometheus
  namespace  = kubernetes_namespace.this["observability"].metadata[0].name

  atomic          = true
  cleanup_on_fail = true
  # The stack installs a large CRD set and several components; the default
  # 5-minute timeout expires on a cold laptop before Prometheus is ready.
  # Raised from 900 on Day 6. On a laptop with Docker capped at ~5 GiB, image
  # pulls and scheduling on a cold cluster routinely exceed the old value, and
  # atomic = true then UNINSTALLS a release whose pods were already Running --
  # so bootstrap failed while the cluster was in the middle of coming up
  # correctly. A generous timeout costs nothing when things are fast.
  timeout = 2400
  values = [yamlencode({
    # ----------------------------------------------------------------------
    # Prometheus
    # ----------------------------------------------------------------------
    prometheus = {
      prometheusSpec = {
        retention      = local.effective.prometheus_retention
        scrapeInterval = "15s"

        # Without this, the operator only picks up ServiceMonitors carrying the
        # release label — so the app's own ServiceMonitor, installed by a
        # different Helm release, would be silently ignored. This is the single
        # most common reason "my metrics do not appear in Prometheus".
        serviceMonitorSelectorNilUsesHelmValues = false
        podMonitorSelectorNilUsesHelmValues     = false
        ruleSelectorNilUsesHelmValues           = false
        probeSelectorNilUsesHelmValues          = false

        # Exemplars link a histogram bucket to the trace that produced it.
        # Without this flag the storage is disabled and the trace links on the
        # latency panel silently do nothing.
        enableFeatures = ["exemplar-storage"]

        resources = {
          requests = { cpu = "100m", memory = local.effective.prometheus_memory }
          limits   = { memory = local.effective.prometheus_memory }
        }

        storageSpec = {
          volumeClaimTemplate = {
            spec = {
              accessModes = ["ReadWriteOnce"]
              resources   = { requests = { storage = local.effective.prometheus_storage_size } }
            }
          }
        }
      }
    }

    # ----------------------------------------------------------------------
    # Alertmanager
    # ----------------------------------------------------------------------
    alertmanager = {
      alertmanagerSpec = {
        resources = {
          requests = { cpu = "10m", memory = "64Mi" }
          limits   = { memory = "128Mi" }
        }
      }
      config = {
        global = { resolve_timeout = "5m" }
        route = {
          group_by        = ["alertname", "namespace"]
          group_wait      = "30s"
          group_interval  = "5m"
          repeat_interval = "4h"
          # Everything goes to a webhook receiver running in-cluster. No email,
          # no Slack, no PagerDuty — all of those need an account, and the
          # point is to prove routing works, not to own a paging rotation.
          receiver = "local-webhook"
          routes = [{
            matchers = ["severity=\"critical\""]
            receiver = "local-webhook"
            # Critical alerts repeat hourly instead of every four hours.
            repeat_interval = "1h"
          }]
        }
        receivers = [{
          name = "local-webhook"
          webhook_configs = [{
            url           = "http://alert-receiver.observability.svc.cluster.local:9095/alerts"
            send_resolved = true
          }]
        }]
        inhibit_rules = [{
          # A critical alert suppresses the warning for the same thing, so an
          # incident does not arrive as two pages.
          source_matchers = ["severity=\"critical\""]
          target_matchers = ["severity=\"warning\""]
          equal           = ["alertname", "namespace"]
        }]
      }
    }

    # ----------------------------------------------------------------------
    # Grafana
    # ----------------------------------------------------------------------
    grafana = {
      adminPassword = local.grafana_password

      resources = {
        requests = { cpu = "50m", memory = "128Mi" }
        limits   = { memory = "256Mi" }
      }

      "grafana.ini" = {
        analytics = {
          reporting_enabled = false
          check_for_updates = false
        }
        users = { default_theme = "dark" }
      }

      # Datasources beyond Prometheus, which the chart wires up itself.
      additionalDataSources = concat(
        local.effective.enable_tempo ? [{
          name = "Tempo"
          type = "tempo"
          uid  = "tempo"
          url  = "http://tempo.observability.svc.cluster.local:3200"
          jsonData = {
            tracesToLogsV2 = local.effective.enable_loki ? {
              datasourceUid      = "loki"
              spanStartTimeShift = "-5m"
              spanEndTimeShift   = "5m"
              filterByTraceID    = true
            } : null
          }
        }] : [],
        local.effective.enable_loki ? [{
          name = "Loki"
          type = "loki"
          uid  = "loki"
          url  = "http://loki-gateway.observability.svc.cluster.local"
          jsonData = {
            derivedFields = [{
              name          = "TraceID"
              matcherType   = "label"
              matcherRegex  = "trace_id"
              url           = "$${__value.raw}"
              datasourceUid = "tempo"
            }]
          }
        }] : []
      )

      # Exemplars are configured through the chart's OWN Prometheus datasource,
      # via the sidecar setting, rather than by declaring a second datasource.
      #
      # An earlier version defined a full `datasources` block with
      # isDefault: true. kube-prometheus-stack already creates a default
      # Prometheus datasource, so that produced TWO defaults — and Grafana
      # refuses to start with "Only one datasource per organization can be
      # marked as default". It presents as a CrashLoopBackOff with the real
      # reason buried in the provisioning log.
      sidecar = {
        datasources = {
          enabled                  = true
          defaultDatasourceEnabled = true
          # This is what turns an exemplar on the latency panel into a link to
          # the trace that produced it.
          exemplarTraceIdDestinations = local.effective.enable_tempo ? {
            datasourceUid    = "tempo"
            traceIdLabelName = "trace_id"
          } : null
        }

        # The Day 3 dashboard, unchanged, loaded from a ConfigMap.
        dashboards = {
          enabled          = true
          label            = "grafana_dashboard"
          searchNamespace  = "ALL"
          folderAnnotation = "grafana_folder"
          provider         = { foldersFromFilesStructure = true }
        }
      }

      ingress = {
        enabled          = true
        ingressClassName = "nginx"
        hosts            = ["grafana.localhost"]
        path             = "/"
        pathType         = "Prefix"
      }
    }

    # ----------------------------------------------------------------------
    # Components trimmed for a laptop
    # ----------------------------------------------------------------------
    nodeExporter = {
      enabled = !local.lowmem
    }
    prometheus-node-exporter = {
      resources = {
        requests = { cpu = "10m", memory = "32Mi" }
        limits   = { memory = "64Mi" }
      }
    }
    kubeStateMetrics = { enabled = true }
    kube-state-metrics = {
      resources = {
        requests = { cpu = "10m", memory = "64Mi" }
        limits   = { memory = "128Mi" }
      }
    }

    # k3s does not expose these the way a kubeadm cluster does, and leaving
    # them enabled produces permanently-down scrape targets that train an
    # operator to ignore red.
    kubeControllerManager = { enabled = false }
    kubeScheduler         = { enabled = false }
    kubeProxy             = { enabled = false }
    kubeEtcd              = { enabled = false }

    defaultRules = {
      create = true
      rules = {
        # These fire constantly on a single-node k3d cluster and teach nobody
        # anything.
        kubeApiserverSlos     = false
        kubeApiserverBurnrate = false
        etcd                  = false
      }
    }
  })]

  depends_on = [kubernetes_namespace.this]
}

# ---------------------------------------------------------------------------
# Tempo
# ---------------------------------------------------------------------------

resource "helm_release" "tempo" {
  count = local.effective.enable_tempo ? 1 : 0

  name       = "tempo"
  repository = "https://grafana.github.io/helm-charts"
  chart      = "tempo"
  version    = var.chart_versions.tempo
  namespace  = kubernetes_namespace.this["observability"].metadata[0].name

  atomic          = true
  cleanup_on_fail = true
  timeout         = 600

  values = [yamlencode({
    tempo = {
      retention = "24h"
      resources = {
        requests = { cpu = "50m", memory = "192Mi" }
        limits   = { memory = "384Mi" }
      }
      receivers = {
        otlp = {
          protocols = {
            grpc = { endpoint = "0.0.0.0:4317" }
            http = { endpoint = "0.0.0.0:4318" }
          }
        }
      }
      storage = {
        trace = {
          backend = "local"
          local   = { path = "/var/tempo/traces" }
          wal     = { path = "/var/tempo/wal" }
        }
      }
    }
    persistence = {
      enabled = true
      size    = "2Gi"
    }
    serviceMonitor = { enabled = true }
  })]

  depends_on = [helm_release.kube_prometheus_stack]
}

# ---------------------------------------------------------------------------
# Loki
# ---------------------------------------------------------------------------

resource "helm_release" "loki" {
  count = local.effective.enable_loki ? 1 : 0

  name       = "loki"
  repository = "https://grafana.github.io/helm-charts"
  chart      = "loki"
  version    = var.chart_versions.loki
  namespace  = kubernetes_namespace.this["observability"].metadata[0].name

  atomic          = true
  cleanup_on_fail = true
  timeout         = 900

  values = [yamlencode({
    deploymentMode = "SingleBinary"

    loki = {
      auth_enabled = false
      commonConfig = { replication_factor = 1 }
      storage      = { type = "filesystem" }
      schemaConfig = {
        configs = [{
          from         = "2024-01-01"
          store        = "tsdb"
          object_store = "filesystem"
          schema       = "v13"
          index        = { prefix = "index_", period = "24h" }
        }]
      }
      limits_config = {
        retention_period          = "24h"
        allow_structured_metadata = true
        # Containers emit slightly out-of-order timestamps under load;
        # rejecting them would silently drop the logs from a busy moment.
        reject_old_samples = false
      }
      compactor = {
        retention_enabled    = true
        delete_request_store = "filesystem"
      }
    }

    singleBinary = {
      replicas = 1
      resources = {
        requests = { cpu = "50m", memory = "192Mi" }
        limits   = { memory = "384Mi" }
      }
      persistence = { enabled = true, size = "2Gi" }
    }

    # The distributed-mode components must be explicitly zeroed, or the chart
    # schedules them alongside the single binary and nothing becomes ready.
    read         = { replicas = 0 }
    write        = { replicas = 0 }
    backend      = { replicas = 0 }
    chunksCache  = { enabled = false }
    resultsCache = { enabled = false }

    lokiCanary = { enabled = false }
    test       = { enabled = false }
    monitoring = { selfMonitoring = { enabled = false, grafanaAgent = { installOperator = false } } }
  })]

  depends_on = [helm_release.kube_prometheus_stack]
}

output "grafana_admin_password" {
  description = "Grafana admin password."
  value       = local.grafana_password
  sensitive   = true
}
