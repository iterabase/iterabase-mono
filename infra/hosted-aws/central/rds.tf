# Central store (DES-HOR-661-08): registry, usage, FX, alerts and rate counters.
# Multi-AZ for automatic failover; never reachable from outside the task SG.
resource "aws_db_subnet_group" "central" {
  name       = local.name
  subnet_ids = local.subnet_ids
}

resource "aws_db_parameter_group" "central" {
  name   = local.name
  family = "postgres17"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
}

resource "aws_db_instance" "central" {
  identifier     = local.name
  engine         = "postgres"
  engine_version = "17"
  instance_class = "db.t4g.small"

  db_name                     = "central"
  username                    = "central"
  manage_master_user_password = true

  multi_az              = true
  storage_type          = "gp3"
  allocated_storage     = 20
  max_allocated_storage = 100
  storage_encrypted     = true

  db_subnet_group_name   = aws_db_subnet_group.central.name
  parameter_group_name   = aws_db_parameter_group.central.name
  vpc_security_group_ids = [aws_security_group.db.id]
  publicly_accessible    = false

  backup_retention_period    = 7
  copy_tags_to_snapshot      = true
  deletion_protection        = true
  skip_final_snapshot        = false
  final_snapshot_identifier  = "${local.name}-final"
  auto_minor_version_upgrade = true
}
