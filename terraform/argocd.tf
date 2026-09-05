# ArgoCD.
#
# The cluster is provisioned by Terraform; the APPLICATION is deployed by
# ArgoCD from Git. That split is deliberate and worth stating, because the
# obvious alternative — have Terraform install the app's Helm chart too — is
# what most people do and it is worse:
#
#   * Terraform would own the app's desired state, so a `kubectl edit` drifts
#     silently until the next apply, which might be weeks away.
#   * Deploying a new image tag would mean running Terraform from CI, which
#     means CI holds cluster credentials.
#   * Rollback would be "revert the commit and re-run apply", with no record of
#     what was actually running when.
#
# ArgoCD reconciles continuously, so drift is corrected in seconds rather than
# at the next apply, and it pulls from Git rather than being pushed to — so no
# credential ever leaves the cluster.

resource "helm_release" "argocd" {
  name       = "argocd"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argo-cd"
  version    = var.chart_versions.argo_cd
  namespace  = kubernetes_namespace.this["argocd"].metadata[0].name

  atomic          = true
  cleanup_on_fail = true
  # Raised from 900 on Day 6. On a laptop with Docker capped at ~5 GiB, image
  # pulls and scheduling on a cold cluster routinely exceed the old value, and
  # atomic = true then UNINSTALLS a release whose pods were already Running --
  # so bootstrap failed while the cluster was in the middle of coming up
  # correctly. A generous timeout costs nothing when things are fast.
  timeout = 2400
  values = [yamlencode({
    global = {
      # The default is 'normal', which on a small cluster is mostly reconcile
      # chatter from the application controller.
      logging = { level = "warn" }
    }

    configs = {
      params = {
        # No TLS on the ArgoCD server: it sits behind ingress-nginx inside a
        # laptop cluster, and terminating TLS twice means managing a
        # certificate for a hop that never leaves the host.
        "server.insecure" = true
        # Faster reconcile than the 3-minute default, so the self-healing demo
        # does not require waiting around. Costs API server load, which is
        # irrelevant at this scale.
        "timeout.reconciliation" = "30s"
      }

      cm = {
        # Let Applications target namespaces other than argocd.
        "application.resourceTrackingMethod" = "annotation"
        # Kustomize and Helm both enabled; the app-of-apps uses plain
        # directory-of-manifests, the child app uses Helm.
        "kustomize.buildOptions" = "--enable-helm"
      }

      rbac = {
        # Read-only for anyone not explicitly granted more. The admin account
        # is used by the bootstrap and by the runbook's rollback command.
        "policy.default" = "role:readonly"
      }
    }

    server = {
      resources = {
        requests = { cpu = "50m", memory = "128Mi" }
        limits   = { memory = "256Mi" }
      }
      ingress = {
        enabled          = true
        ingressClassName = "nginx"
        hostname         = "argocd.localhost"
      }
    }

    controller = {
      # The application controller is the memory-hungry component: it holds a
      # cache of every object it manages. On this cluster that is a few
      # hundred objects, so the default 1Gi request would reserve far more than
      # it can use.
      resources = {
        requests = { cpu = "100m", memory = "256Mi" }
        limits   = { memory = "512Mi" }
      }
    }

    repoServer = {
      resources = {
        requests = { cpu = "50m", memory = "192Mi" }
        limits   = { memory = "384Mi" }
      }
    }

    redis = {
      resources = {
        requests = { cpu = "20m", memory = "64Mi" }
        limits   = { memory = "128Mi" }
      }
    }

    applicationSet = {
      # Not used — the app-of-apps pattern covers this project's needs, and
      # ApplicationSet is another controller to keep resident.
      enabled = false
    }

    dex = {
      # SSO would need an identity provider, which would need an account.
      enabled = false
    }

    notifications = {
      enabled = false
    }
  })]

  depends_on = [
    kubernetes_namespace.this,
    helm_release.ingress_nginx,
  ]
}

# The root Application. Everything else ArgoCD manages is declared in Git under
# /gitops and discovered from here — the "app of apps" pattern.
#
# This single resource is the ONLY thing Terraform tells ArgoCD about. Adding a
# new component later is a file in /gitops, not a Terraform change, which is
# what keeps the deployment boundary clean.
resource "kubectl_manifest" "root_application" {
  depends_on = [helm_release.argocd]

  yaml_body = yamlencode({
    apiVersion = "argoproj.io/v1alpha1"
    kind       = "Application"
    metadata = {
      name      = "root"
      namespace = kubernetes_namespace.this["argocd"].metadata[0].name
      # Without this finalizer, deleting the root Application orphans every
      # child instead of cascading.
      finalizers = ["resources-finalizer.argocd.argoproj.io"]
    }
    spec = {
      project = "default"
      source = {
        repoURL        = var.gitops_repo_url
        targetRevision = var.gitops_target_revision
        path           = "gitops/apps"
      }
      destination = {
        server    = "https://kubernetes.default.svc"
        namespace = kubernetes_namespace.this["argocd"].metadata[0].name
      }
      syncPolicy = {
        automated = {
          prune    = true
          selfHeal = true
        }
        syncOptions = ["CreateNamespace=false"]
      }
    }
  })
}

output "argocd_admin_password_command" {
  description = "How to read the generated ArgoCD admin password."
  value       = "kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"
}
