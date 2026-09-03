# Chaos Mesh, installed now and used on Day 5.
#
# Installed in Terraform rather than through ArgoCD because it is cluster
# infrastructure, not application deployment: it needs privileged access to
# manipulate network and process state on the node, and its lifecycle is tied
# to the cluster rather than to a release of this service.

resource "helm_release" "chaos_mesh" {
  count = local.effective.enable_chaos_mesh ? 1 : 0

  name       = "chaos-mesh"
  repository = "https://charts.chaos-mesh.org"
  chart      = "chaos-mesh"
  version    = var.chart_versions.chaos_mesh
  namespace  = kubernetes_namespace.this["chaos-testing"].metadata[0].name

  atomic          = true
  cleanup_on_fail = true
  timeout         = 900

  values = [yamlencode({
    chaosDaemon = {
      # k3s uses containerd, and the daemon needs its socket to enter a
      # container's network namespace. The default assumes Docker and silently
      # fails to inject anything.
      runtime    = "containerd"
      socketPath = "/run/k3s/containerd/containerd.sock"
      resources = {
        requests = { cpu = "10m", memory = "64Mi" }
        limits   = { memory = "128Mi" }
      }
    }

    controllerManager = {
      replicaCount = 1
      resources = {
        requests = { cpu = "25m", memory = "128Mi" }
        limits   = { memory = "256Mi" }
      }
    }

    dashboard = {
      create = true
      # No persistence: experiment history is not worth a PVC here, and the
      # experiments themselves are committed as YAML on Day 5.
      persistentVolume = { enabled = false }
      securityMode     = false
      resources = {
        requests = { cpu = "10m", memory = "64Mi" }
        limits   = { memory = "128Mi" }
      }
      ingress = {
        enabled = true
        annotations = {
          "kubernetes.io/ingress.class" = "nginx"
        }
        hosts = [{ name = "chaos.localhost" }]
      }
    }

    dnsServer = {
      # Needed for DNS chaos on Day 5 — failing name resolution is one of the
      # more realistic ways an endpoint becomes unreachable.
      create = true
      resources = {
        requests = { cpu = "10m", memory = "32Mi" }
        limits   = { memory = "64Mi" }
      }
    }

    prometheus = {
      # kube-prometheus-stack already scrapes this via a ServiceMonitor.
      create = false
    }
  })]

  depends_on = [
    kubernetes_namespace.this,
    helm_release.kube_prometheus_stack,
  ]
}
