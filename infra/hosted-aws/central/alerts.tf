# Iterabase's copy of the 80%/100% credit alerts (DES-HOR-661-06). Installations
# receive theirs in the mTLS installation read.
resource "aws_sns_topic" "billing_alerts" {
  name = "${local.name}-billing-alerts"
}

resource "aws_sns_topic_subscription" "billing_alerts_email" {
  topic_arn = aws_sns_topic.billing_alerts.arn
  protocol  = "email"
  endpoint  = var.billing_alert_email
}

# Operations alarms for the service every managed installation depends on.
resource "aws_sns_topic" "ops_alerts" {
  name = "${local.name}-ops-alerts"
}

resource "aws_sns_topic_subscription" "ops_alerts_email" {
  topic_arn = aws_sns_topic.ops_alerts.arn
  protocol  = "email"
  endpoint  = var.ops_alert_email
}

locals {
  alarms = {
    rds-cpu-credits = {
      description = "Central RDS is running out of burst CPU credits (DES-HOR-661-07)."
      namespace   = "AWS/RDS"
      metric      = "CPUCreditBalance"
      statistic   = "Minimum"
      comparison  = "LessThanThreshold"
      threshold   = 30
      dimensions  = { DBInstanceIdentifier = aws_db_instance.central.identifier }
    }
    rds-free-storage = {
      description = "Central RDS free storage below 2 GiB."
      namespace   = "AWS/RDS"
      metric      = "FreeStorageSpace"
      statistic   = "Minimum"
      comparison  = "LessThanThreshold"
      threshold   = 2147483648
      dimensions  = { DBInstanceIdentifier = aws_db_instance.central.identifier }
    }
    running-tasks = {
      description = "Fewer central replicas running than desired."
      namespace   = "ECS/ContainerInsights"
      metric      = "RunningTaskCount"
      statistic   = "Minimum"
      comparison  = "LessThanThreshold"
      threshold   = var.task_count
      dimensions  = { ClusterName = aws_ecs_cluster.central.name, ServiceName = aws_ecs_service.central.name }
    }
    unhealthy-targets = {
      description = "A central replica fails the NLB /readyz check."
      namespace   = "AWS/NetworkELB"
      metric      = "UnHealthyHostCount"
      statistic   = "Maximum"
      comparison  = "GreaterThanThreshold"
      threshold   = 0
      dimensions  = { LoadBalancer = aws_lb.central.arn_suffix, TargetGroup = aws_lb_target_group.central.arn_suffix }
    }
    fx-rate-unavailable = {
      description = "Central could not lock a needed ECB FX rate; FX-dependent managed inference is paused (DES-HOR-661-04)."
      namespace   = "Iterabase/Central"
      metric      = "FxRateUnavailable"
      statistic   = "Sum"
      comparison  = "GreaterThanThreshold"
      threshold   = 0
      dimensions  = {}
    }
  }
}

resource "aws_cloudwatch_log_metric_filter" "fx_rate_unavailable" {
  name           = "${local.name}-fx-rate-unavailable"
  log_group_name = aws_cloudwatch_log_group.central.name
  pattern        = "\"fx_rate_unavailable\""

  metric_transformation {
    name          = "FxRateUnavailable"
    namespace     = "Iterabase/Central"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_metric_alarm" "central" {
  for_each = local.alarms

  alarm_name          = "${local.name}-${each.key}"
  alarm_description   = each.value.description
  namespace           = each.value.namespace
  metric_name         = each.value.metric
  statistic           = each.value.statistic
  comparison_operator = each.value.comparison
  threshold           = each.value.threshold
  dimensions          = each.value.dimensions
  period              = 300
  evaluation_periods  = 1
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.ops_alerts.arn]
  ok_actions          = [aws_sns_topic.ops_alerts.arn]
}
