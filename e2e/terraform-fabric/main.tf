# Unmodified microsoft/fabric resources against the emulator.
# Auth and endpoint come from FABRIC_* environment variables — the same
# contract a developer uses with a real tenant. preview is required because
# fabric_folder is still a preview resource in this provider version.
terraform {
  required_version = ">= 1.8, < 2.0"
  required_providers {
    fabric = {
      source  = "microsoft/fabric"
      version = "1.12.1"
    }
  }
}

provider "fabric" {
  preview = true
}

data "fabric_capacity" "emulator" {
  display_name = "Emulator Capacity"
}

resource "fabric_workspace" "dev" {
  display_name = "tf-witness"
  description  = "terraform-provider-fabric against the emulator"
  capacity_id  = data.fabric_capacity.emulator.id
}

resource "fabric_folder" "src" {
  display_name = "src"
  workspace_id = fabric_workspace.dev.id
}

resource "fabric_folder" "ingest" {
  display_name     = "ingest"
  workspace_id     = fabric_workspace.dev.id
  parent_folder_id = fabric_folder.src.id
}

resource "fabric_lakehouse" "lh" {
  display_name = "tflh"
  workspace_id = fabric_workspace.dev.id
  folder_id    = fabric_folder.ingest.id
}

resource "fabric_workspace_role_assignment" "viewer" {
  workspace_id = fabric_workspace.dev.id
  principal = {
    id   = "11111111-1111-1111-1111-111111111111"
    type = "User"
  }
  role = "Viewer"
}

output "workspace_id" {
  value = fabric_workspace.dev.id
}

output "capacity_id" {
  value = data.fabric_capacity.emulator.id
}

output "capacity_state" {
  value = data.fabric_capacity.emulator.state
}

output "folder_id" {
  value = fabric_folder.ingest.id
}

output "lakehouse_id" {
  value = fabric_lakehouse.lh.id
}

output "role_assignment_id" {
  value = fabric_workspace_role_assignment.viewer.id
}
