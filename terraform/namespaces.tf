# Namespaces are created here rather than by each Helm release's
# create_namespace, so that labels the NetworkPolicies depend on are guaranteed
# to exist before anything selects on them — and so `terraform destroy` removes
# them in a deterministic order.

resource "kubernetes_namespace" "this" {
  for_each = var.namespaces

  metadata {
    name   = each.key
    labels = merge(local.common_labels, each.value.labels)
  }
}
