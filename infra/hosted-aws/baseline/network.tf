# One shared VPC with a public subnet per AZ. Customer nodes (HOR-593) get a
# public address for egress and the in-cluster AWS Load Balancer Controller
# (HOR-601) discovers these subnets by the elb role tag. Security groups admit no
# inbound SSH; access is SSM over the public internet, so the SSM interface
# endpoints are deliberately not created (instances already have egress).
resource "aws_vpc" "shared" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = "${local.name}-shared"
  }
}

resource "aws_internet_gateway" "shared" {
  vpc_id = aws_vpc.shared.id

  tags = {
    Name = "${local.name}-shared"
  }
}

resource "aws_subnet" "public" {
  for_each = { for index, zone in var.availability_zones : zone => index }

  vpc_id                  = aws_vpc.shared.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.vpc_cidr, 4, each.value)
  map_public_ip_on_launch = true

  tags = {
    Name                     = "${local.name}-public-${each.key}"
    "kubernetes.io/role/elb" = "1"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.shared.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.shared.id
  }

  tags = {
    Name = "${local.name}-public"
  }
}

resource "aws_route_table_association" "public" {
  for_each = aws_subnet.public

  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

# Backups and state stay on the AWS network at no charge.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.shared.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.public.id]

  tags = {
    Name = "${local.name}-s3"
  }
}

# The default security group admits nothing; every workload gets its own.
resource "aws_default_security_group" "shared" {
  vpc_id = aws_vpc.shared.id

  tags = {
    Name = "${local.name}-default-deny"
  }
}
