# VPC with two AZs: public subnets for the ALB and a NAT instance, private
# subnets for Fargate tasks, Redis and Aurora. fck-nat on a t4g.nano is
# the £47/mo floor instead of a managed NAT Gateway ($35/mo).
# shortcut: single NAT instance, no HA. Swap for a managed NAT Gateway
# if you move this stack out of demo mode.

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 2)
}

resource "aws_vpc" "main" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "${var.name_prefix}-vpc" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${var.name_prefix}-igw" }
}

resource "aws_subnet" "public" {
  count                   = 2
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.42.${count.index}.0/24"
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = true
  tags                    = { Name = "${var.name_prefix}-public-${count.index}" }
}

resource "aws_subnet" "private" {
  count             = 2
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.42.${count.index + 10}.0/24"
  availability_zone = local.azs[count.index]
  tags              = { Name = "${var.name_prefix}-private-${count.index}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
  tags = { Name = "${var.name_prefix}-public" }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# fck-nat AMI: minimal Amazon Linux 2023 image that NATs on a nano instance.
data "aws_ami" "fck_nat" {
  most_recent = true
  owners      = ["568608671756"] # fck-nat publisher
  filter {
    name   = "name"
    values = ["fck-nat-al2023-hvm-*-arm64-ebs"]
  }
}

resource "aws_security_group" "nat" {
  name   = "${var.name_prefix}-nat"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_network_interface" "nat" {
  subnet_id         = aws_subnet.public[0].id
  security_groups   = [aws_security_group.nat.id]
  source_dest_check = false
}

resource "aws_instance" "nat" {
  ami                  = data.aws_ami.fck_nat.id
  instance_type        = "t4g.nano"
  iam_instance_profile = aws_iam_instance_profile.nat.name
  network_interface {
    device_index         = 0
    network_interface_id = aws_network_interface.nat.id
  }
  tags = { Name = "${var.name_prefix}-nat" }
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block           = "0.0.0.0/0"
    network_interface_id = aws_network_interface.nat.id
  }
  tags = { Name = "${var.name_prefix}-private" }
}

resource "aws_route_table_association" "private" {
  count          = 2
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# Gateway VPC endpoint for DynamoDB keeps the spend-write path off the
# NAT instance and off the public internet entirely -- the one piece
# of the AWS Free-Tier always-free promise this stack actually leans on.
resource "aws_vpc_endpoint" "dynamodb" {
  vpc_id          = aws_vpc.main.id
  service_name    = "com.amazonaws.${var.region}.dynamodb"
  route_table_ids = [aws_route_table.private.id]
}

# ---- ALB ----

resource "aws_security_group" "alb" {
  name   = "${var.name_prefix}-alb"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_lb" "gateway" {
  name               = "${var.name_prefix}-alb"
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id
}

resource "aws_lb_target_group" "gateway" {
  name        = "${var.name_prefix}-gateway"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.main.id
  health_check {
    path                = "/healthz"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 15
    timeout             = 5
    matcher             = "200"
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.gateway.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway.arn
  }
}

# ---- Service security groups ----

resource "aws_security_group" "fargate" {
  name   = "${var.name_prefix}-fargate"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port       = 8080
    to_port         = 8082
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }
  ingress {
    # Intra-VPC service-to-service (controlplane → advisor, etc.)
    from_port = 8080
    to_port   = 8082
    protocol  = "tcp"
    self      = true
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "redis" {
  name   = "${var.name_prefix}-redis"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_security_group.fargate.id]
  }
}

resource "aws_security_group" "postgres" {
  name   = "${var.name_prefix}-postgres"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.fargate.id]
  }
}
