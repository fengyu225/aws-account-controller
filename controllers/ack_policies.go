package controllers

// ACKServicePolicies contains the recommended managed and inline policies for each ACK service
type ACKServicePolicies struct {
	ManagedPolicyARNs []string
	InlinePolicy      string
}

// ACKPolicies maps service names to their recommended policies
var ACKPolicies = map[string]ACKServicePolicies{
	"eks": {
		ManagedPolicyARNs: []string{},
		InlinePolicy: `{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Effect": "Allow",
					"Action": [
						"eks:*",
						"iam:GetRole",
						"iam:PassRole",
						"iam:ListAttachedRolePolicies",
						"ec2:DescribeSubnets"
					],
					"Resource": "*"
				}
			]
		}`,
	},
	"s3": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonS3FullAccess",
		},
		InlinePolicy: `{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Sid": "S3AllPermission",
					"Effect": "Allow",
					"Action": [
						"s3:*",
						"s3-object-lambda:*"
					],
					"Resource": "*"
				},
				{
					"Sid": "S3ReplicationPassRole",
					"Condition": {
						"StringEquals": {
							"iam:PassedToService": "s3.amazonaws.com"
						}
					},
					"Action": "iam:PassRole",
					"Resource": "*",
					"Effect": "Allow"
				}
			]
		}`,
	},
	"rds": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonRDSFullAccess",
		},
		InlinePolicy: "",
	},
	"dynamodb": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonDynamoDBFullAccess",
		},
		InlinePolicy: "",
	},
	"ec2": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonEC2FullAccess",
		},
		InlinePolicy: "",
	},
	"ecr": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryFullAccess",
		},
		InlinePolicy: "",
	},
	"iam": {
		ManagedPolicyARNs: []string{},
		InlinePolicy: `{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Sid": "VisualEditor0",
					"Effect": "Allow",
					"Action": [
						"iam:GetGroup",
						"iam:CreateGroup",
						"iam:DeleteGroup",
						"iam:UpdateGroup",
						"iam:GetRole",
						"iam:CreateRole",
						"iam:DeleteRole",
						"iam:UpdateRole",
						"iam:PutRolePermissionsBoundary",
						"iam:PutUserPermissionsBoundary",
						"iam:GetUser",
						"iam:CreateUser",
						"iam:DeleteUser",
						"iam:UpdateUser",
						"iam:GetPolicy",
						"iam:CreatePolicy",
						"iam:DeletePolicy",
						"iam:GetPolicyVersion",
						"iam:CreatePolicyVersion",
						"iam:DeletePolicyVersion",
						"iam:ListPolicyVersions",
						"iam:ListPolicyTags",
						"iam:ListAttachedGroupPolicies",
						"iam:GetGroupPolicy",
						"iam:PutGroupPolicy",
						"iam:AttachGroupPolicy",
						"iam:DetachGroupPolicy",
						"iam:DeleteGroupPolicy",
						"iam:ListAttachedRolePolicies",
						"iam:ListRolePolicies",
						"iam:GetRolePolicy",
						"iam:PutRolePolicy",
						"iam:AttachRolePolicy",
						"iam:DetachRolePolicy",
						"iam:DeleteRolePolicy",
						"iam:ListAttachedUserPolicies",
						"iam:ListUserPolicies",
						"iam:GetUserPolicy",
						"iam:PutUserPolicy",
						"iam:AttachUserPolicy",
						"iam:DetachUserPolicy",
						"iam:DeleteUserPolicy",
						"iam:ListRoleTags",
						"iam:ListUserTags",
						"iam:TagPolicy",
						"iam:UntagPolicy",
						"iam:TagRole",
						"iam:UntagRole",
						"iam:TagUser",
						"iam:UntagUser",
						"iam:RemoveClientIDFromOpenIDConnectProvider",
						"iam:ListOpenIDConnectProviderTags",
						"iam:UpdateOpenIDConnectProviderThumbprint",
						"iam:UntagOpenIDConnectProvider",
						"iam:AddClientIDToOpenIDConnectProvider",
						"iam:DeleteOpenIDConnectProvider",
						"iam:GetOpenIDConnectProvider",
						"iam:TagOpenIDConnectProvider",
						"iam:CreateOpenIDConnectProvider",
						"iam:UpdateAssumeRolePolicy",
						"iam:CreateServiceLinkedRole"
					],
					"Resource": "*"
				}
			]
		}`,
	},
	"lambda": {
		ManagedPolicyARNs: []string{},
		InlinePolicy: `{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Effect": "Allow",
					"Action": [
						"lambda:*",
						"s3:Get*",
						"ecr:Get*",
						"ecr:BatchGet*",
						"ec2:DescribeSecurityGroups",
						"ec2:DescribeSubnets",
						"ec2:DescribeVpcs"
					],
					"Resource": "*"
				},
				{
					"Action": "iam:PassRole",
					"Condition": {
						"StringEquals": {
							"iam:PassedToService": "lambda.amazonaws.com"
						}
					},
					"Effect": "Allow",
					"Resource": "*"
				}
			]
		}`,
	},
	"sqs": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonSQSFullAccess",
		},
		InlinePolicy: "",
	},
	"sns": {
		ManagedPolicyARNs: []string{
			"arn:aws:iam::aws:policy/AmazonSNSFullAccess",
		},
		InlinePolicy: "",
	},
}
