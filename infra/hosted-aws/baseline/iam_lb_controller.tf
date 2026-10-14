# DES-HOR-601-02: the AWS Load Balancer Controller's policy, attached by HOR-593
# to every per-customer node role. Adapted from the upstream controller policy
# for instance-mode NLBs, with the upstream cluster-tag conditions replaced by
# environment isolation: a node may only create resources tagged with its own
# role's iterabase-env tag, and may only change or delete resources that already
# carry it. The node boundary (DES-HOR-603-08) still caps everything here.
locals {
  # Literal IAM policy variable, resolved per request from the node role's tag.
  own_env = "$${aws:PrincipalTag/${local.env_tag_key}}"

  lb_controller_retag_resources = [
    "arn:${local.partition}:elasticloadbalancing:${var.region}:${local.account_id}:loadbalancer/*",
    "arn:${local.partition}:elasticloadbalancing:${var.region}:${local.account_id}:targetgroup/*",
    "arn:${local.partition}:elasticloadbalancing:${var.region}:${local.account_id}:listener/*",
    "arn:${local.partition}:elasticloadbalancing:${var.region}:${local.account_id}:listener-rule/*",
  ]
  lb_controller_security_groups = "arn:${local.partition}:ec2:${var.region}:${local.account_id}:security-group/*"
}

data "aws_iam_policy_document" "lb_controller" {
  statement {
    sid       = "LoadBalancerServiceLinkedRole"
    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "iam:AWSServiceName"
      values   = ["elasticloadbalancing.amazonaws.com"]
    }
  }

  # Discovery: subnets by the elb role tag, the node's instance and security
  # groups, and the controller's own resources. Read-only and untaggable.
  statement {
    sid = "Discover"
    actions = [
      "ec2:DescribeAccountAttributes",
      "ec2:DescribeAddresses",
      "ec2:DescribeAvailabilityZones",
      "ec2:DescribeInternetGateways",
      "ec2:DescribeVpcs",
      "ec2:DescribeSubnets",
      "ec2:DescribeSecurityGroups",
      "ec2:DescribeInstances",
      "ec2:DescribeNetworkInterfaces",
      "ec2:DescribeTags",
      "ec2:DescribeRouteTables",
      "ec2:GetSecurityGroupsForVpc",
      "elasticloadbalancing:DescribeLoadBalancers",
      "elasticloadbalancing:DescribeLoadBalancerAttributes",
      "elasticloadbalancing:DescribeListeners",
      "elasticloadbalancing:DescribeListenerAttributes",
      "elasticloadbalancing:DescribeListenerCertificates",
      "elasticloadbalancing:DescribeSSLPolicies",
      "elasticloadbalancing:DescribeRules",
      "elasticloadbalancing:DescribeTargetGroups",
      "elasticloadbalancing:DescribeTargetGroupAttributes",
      "elasticloadbalancing:DescribeTargetHealth",
      "elasticloadbalancing:DescribeTags",
      "elasticloadbalancing:DescribeTrustStores",
      "elasticloadbalancing:DescribeCapacityReservation",
      "acm:ListCertificates",
      "acm:DescribeCertificate",
      "shield:GetSubscriptionState",
      "wafv2:GetWebACLForResource",
      "tag:GetResources",
    ]
    resources = ["*"]
  }

  # Security groups only in the shared VPC, and only tagged with the caller's
  # environment at creation.
  statement {
    sid       = "CreateSecurityGroupInSharedVpc"
    actions   = ["ec2:CreateSecurityGroup"]
    resources = [aws_vpc.shared.arn]
  }

  statement {
    sid       = "CreateOwnSecurityGroup"
    actions   = ["ec2:CreateSecurityGroup"]
    resources = [local.lb_controller_security_groups]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = [local.own_env]
    }
  }

  statement {
    sid       = "TagSecurityGroupOnCreate"
    actions   = ["ec2:CreateTags"]
    resources = [local.lb_controller_security_groups]

    condition {
      test     = "StringEquals"
      variable = "ec2:CreateAction"
      values   = ["CreateSecurityGroup"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = [local.own_env]
    }
  }

  # Frontend and backend rules on the environment's own groups, including the
  # node security group HOR-593 creates with the same tag.
  statement {
    sid = "ManageOwnSecurityGroups"
    actions = [
      "ec2:AuthorizeSecurityGroupIngress",
      "ec2:RevokeSecurityGroupIngress",
      "ec2:DeleteSecurityGroup",
    ]
    resources = [local.lb_controller_security_groups]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/${local.env_tag_key}"
      values   = [local.own_env]
    }
  }

  # The controller's ownership tags may change; the environment tag never does,
  # including through a DeleteTags call that names no keys.
  statement {
    sid       = "RetagOwnSecurityGroups"
    actions   = ["ec2:CreateTags", "ec2:DeleteTags"]
    resources = [local.lb_controller_security_groups]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/${local.env_tag_key}"
      values   = [local.own_env]
    }

    condition {
      test     = "ForAllValues:StringNotEquals"
      variable = "aws:TagKeys"
      values   = [local.env_tag_key]
    }

    # ForAllValues passes on an absent key, and DeleteTags without Tags removes
    # every tag: require the request to name its keys.
    condition {
      test     = "Null"
      variable = "aws:TagKeys"
      values   = ["false"]
    }
  }

  statement {
    sid = "CreateOwnLoadBalancers"
    actions = [
      "elasticloadbalancing:CreateLoadBalancer",
      "elasticloadbalancing:CreateTargetGroup",
    ]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = [local.own_env]
    }
  }

  statement {
    sid       = "TagLoadBalancersOnCreate"
    actions   = ["elasticloadbalancing:AddTags"]
    resources = local.lb_controller_retag_resources

    condition {
      test     = "StringEquals"
      variable = "elasticloadbalancing:CreateAction"
      values   = ["CreateTargetGroup", "CreateLoadBalancer", "CreateListener", "CreateRule"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = [local.own_env]
    }
  }

  statement {
    sid = "ManageOwnLoadBalancers"
    actions = [
      "elasticloadbalancing:ModifyLoadBalancerAttributes",
      "elasticloadbalancing:SetIpAddressType",
      "elasticloadbalancing:SetSecurityGroups",
      "elasticloadbalancing:SetSubnets",
      "elasticloadbalancing:DeleteLoadBalancer",
      "elasticloadbalancing:CreateListener",
      "elasticloadbalancing:ModifyListener",
      "elasticloadbalancing:ModifyListenerAttributes",
      "elasticloadbalancing:DeleteListener",
      "elasticloadbalancing:ModifyTargetGroup",
      "elasticloadbalancing:ModifyTargetGroupAttributes",
      "elasticloadbalancing:DeleteTargetGroup",
      "elasticloadbalancing:RegisterTargets",
      "elasticloadbalancing:DeregisterTargets",
    ]
    resources = local.lb_controller_retag_resources

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/${local.env_tag_key}"
      values   = [local.own_env]
    }
  }

  statement {
    sid       = "RetagOwnLoadBalancers"
    actions   = ["elasticloadbalancing:AddTags", "elasticloadbalancing:RemoveTags"]
    resources = local.lb_controller_retag_resources

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/${local.env_tag_key}"
      values   = [local.own_env]
    }

    condition {
      test     = "ForAllValues:StringNotEquals"
      variable = "aws:TagKeys"
      values   = [local.env_tag_key]
    }

    # ForAllValues passes on an absent key, and DeleteTags without Tags removes
    # every tag: require the request to name its keys.
    condition {
      test     = "Null"
      variable = "aws:TagKeys"
      values   = ["false"]
    }
  }
}

resource "aws_iam_policy" "lb_controller" {
  name        = "${local.name}-lb-controller"
  path        = "/iterabase/"
  description = "AWS Load Balancer Controller for per-customer node roles, isolated by iterabase-env (DES-HOR-601-02)"
  policy      = data.aws_iam_policy_document.lb_controller.json
}
