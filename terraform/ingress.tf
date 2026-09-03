# ingress-nginx.
#
# Installed by Terraform rather than using the Traefik that k3s bundles, so the
# ingress is described by code like everything else. k3s's Traefik is disabled
# in the k3d config for the same reason — two controllers both claiming port 80
# is a confusing failure that looks like a networking bug.

resource "helm_release" "ingress_nginx" {
  name       = "ingress-nginx"
  repository = "https://kubernetes.github.io/ingress-nginx"
  chart      = "ingress-nginx"
  version    = var.chart_versions.ingress_nginx
  namespace  = "ingress-nginx"

  create_namespace = true
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 600

  values = [yamlencode({
    controller = {
      # DaemonSet with hostPort, not a LoadBalancer Service: k3d's own
      # loadbalancer container forwards host ports to the nodes, and there is
      # no cloud provider to satisfy a LoadBalancer. A Service of type
      # LoadBalancer would sit Pending forever.
      kind = "DaemonSet"
      hostPort = {
        enabled = true
        ports   = { http = 80, https = 443 }
      }
      service = {
        # The k3d loadbalancer already maps host 8081 -> node 80.
        type = "ClusterIP"
      }

      # Only schedule on agents. The server node runs the control plane, and an
      # ingress controller competing with etcd for memory on a laptop is how
      # you get an unresponsive cluster.
      nodeSelector = {
        "node-role.kubernetes.io/control-plane" = null
      }

      resources = {
        requests = { cpu = "50m", memory = "128Mi" }
        limits   = { memory = "256Mi" }
      }

      metrics = {
        enabled = true
        serviceMonitor = {
          enabled          = true
          namespace        = kubernetes_namespace.this["observability"].metadata[0].name
          additionalLabels = { release = "kube-prometheus-stack" }
        }
      }

      config = {
        # The API accepts payloads up to 256 KiB plus envelope; nginx's 1m
        # default would be fine, but stating it means a payload-size change in
        # the app has one obvious place to match.
        "proxy-body-size" = "2m"
        # Preserve the client's request id if it sent one, so the trace stitches
        # from the edge rather than starting at the app.
        "enable-real-ip"         = "true"
        "use-forwarded-headers"  = "true"
        "log-format-escape-json" = "true"
      }

      admissionWebhooks = {
        # The webhook validates Ingress objects on create. It also adds a Job,
        # a Service and a certificate to every install, and on a single-node
        # laptop cluster it is the most common cause of a stuck `helm install`.
        enabled = false
      }
    }
  })]

  depends_on = [kubernetes_namespace.this]
}
