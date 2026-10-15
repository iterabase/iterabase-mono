# NLB (TCP passthrough: the task terminates installation mTLS) -> tasks -> RDS.
resource "aws_security_group" "nlb" {
  name        = "${local.name}-nlb"
  description = "Central gateway NLB: public TLS passthrough"
  vpc_id      = local.baseline["vpc-id"]
}

resource "aws_security_group" "task" {
  name        = "${local.name}-task"
  description = "Central gateway tasks"
  vpc_id      = local.baseline["vpc-id"]
}

resource "aws_security_group" "db" {
  name        = "${local.name}-db"
  description = "Central gateway database"
  vpc_id      = local.baseline["vpc-id"]
}

resource "aws_vpc_security_group_ingress_rule" "nlb_https" {
  security_group_id = aws_security_group.nlb.id
  description       = "Installations and operators"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "nlb_to_task" {
  for_each = toset([tostring(local.listen_port), tostring(local.health_port)])

  security_group_id            = aws_security_group.nlb.id
  description                  = "Forward and health-check to tasks"
  ip_protocol                  = "tcp"
  from_port                    = tonumber(each.key)
  to_port                      = tonumber(each.key)
  referenced_security_group_id = aws_security_group.task.id
}

resource "aws_vpc_security_group_ingress_rule" "task_from_nlb" {
  for_each = toset([tostring(local.listen_port), tostring(local.health_port)])

  security_group_id            = aws_security_group.task.id
  description                  = "From the NLB only"
  ip_protocol                  = "tcp"
  from_port                    = tonumber(each.key)
  to_port                      = tonumber(each.key)
  referenced_security_group_id = aws_security_group.nlb.id
}

# Egress for the provider, STS, KMS, SNS, ECR and the ECB feed (no NAT: tasks
# have public addresses in the baseline's public subnets).
resource "aws_vpc_security_group_egress_rule" "task_https" {
  security_group_id = aws_security_group.task.id
  description       = "Provider, AWS APIs, ECB"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "task_to_db" {
  security_group_id            = aws_security_group.task.id
  description                  = "Central database"
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.db.id
}

resource "aws_vpc_security_group_ingress_rule" "db_from_task" {
  security_group_id            = aws_security_group.db.id
  description                  = "Central tasks only"
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.task.id
}

resource "aws_lb" "central" {
  name                             = local.name
  load_balancer_type               = "network"
  internal                         = false
  subnets                          = local.subnet_ids
  security_groups                  = [aws_security_group.nlb.id]
  enable_cross_zone_load_balancing = true
}

resource "aws_lb_target_group" "central" {
  name                 = local.name
  port                 = local.listen_port
  protocol             = "TCP"
  target_type          = "ip"
  vpc_id               = local.baseline["vpc-id"]
  deregistration_delay = 120

  health_check {
    protocol            = "HTTP"
    port                = tostring(local.health_port)
    path                = "/readyz"
    healthy_threshold   = 2
    unhealthy_threshold = 2
    interval            = 10
  }
}

resource "aws_lb_listener" "central" {
  load_balancer_arn = aws_lb.central.arn
  port              = 443
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.central.arn
  }
}

resource "cloudflare_dns_record" "central" {
  zone_id = local.baseline["cloudflare-zone-id"]
  name    = local.public_host
  type    = "CNAME"
  content = aws_lb.central.dns_name
  ttl     = 300
  proxied = false
}
