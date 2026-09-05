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
  # Raised from 600 on Day 6. On a laptop with Docker capped at ~5 GiB, image
  # pulls and scheduling on a cold cluster routinely exceed the old value, and
  # atomic = true then UNINSTALLS a release whose pods were already Running --
  # so bootstrap failed while the cluster was in the middle of coming up
  # correctly. A generous timeout costs nothing when things are fast.
  timeout = 1800
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

      # Runs on every node, which is what a DaemonSet with hostPort is for:
      # k3d's loadbalancer forwards the published port to all nodes, so an
      # ingress pod has to exist on each of them.
      #
      # An earlier version tried to exclude the control-plane node with
      # nodeSelector = { "node-role.kubernetes.io/control-plane" = null }.
      # That does NOT mean "where this label is absent" — it renders as
      # `label: ""` and requires the label to EQUAL the empty string. The
      # server node has "true" and the agent has it absent, so it matched
      # nothing: the DaemonSet reported DESIRED 0 and the ingress silently did
      # not exist, while the Helm release still showed "deployed".
      #
      # Expressing "where this label is absent" needs nodeAffinity with a
      # DoesNotExist operator, not a nodeSelector. It is not worth it here —
      # the controller is ~128Mi and halving ingress capacity on a two-node
      # cluster costs more than it saves.

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
