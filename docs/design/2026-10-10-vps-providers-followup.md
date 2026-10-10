# VPS providers: OVHcloud, IONOS and Hexabyte follow-up (2026-10-10)

The owner asked about OVHcloud, Hexabyte and IONOS after the first
survey. This note records what provider pages and public catalog
endpoints gave on 2026-10-10. Pricing pages of OVHcloud and Hetzner
render their figures with JavaScript and could not be read, so
OVHcloud figures come from its public order catalog API and Hetzner
figures stay as in the first survey.

## OVHcloud, VPS line

The public catalog
(<https://api.ovh.com/1.0/order/catalog/public/vps?ovhSubsidiary=IE>)
exposes monthly rental pricing only, no hourly. Datacentres listed:
BHS, DE, GRA, SBG, SGP, SYD, UK, WAW. The product page
(<https://www.ovhcloud.com/en-ie/vps/>) shows VPS-1 (2 vCPU/4 GB) at
€3.81/mo. **UNVERIFIED**: whether a VPS can be ordered and terminated
through the API without a manual order step, cloud-init support, and
whether the `ovh/ovh` Terraform provider
(<https://registry.terraform.io/providers/ovh/ovh/latest/docs>) has a
VPS resource (the registry page could not be read).

## OVHcloud, Public Cloud line

Hourly billing from 60 seconds and an OpenStack API with cloud-init
(<https://www.ovhcloud.com/en-ie/public-cloud/>). From the public
catalog
(<https://api.ovh.com/1.0/order/catalog/public/cloud?ovhSubsidiary=IE>),
plan codes and hourly prices in EUR: `d2-4.consumption` €0.0288/h
(about €21/mo at 730 h), `d2-8.consumption` €0.0576/h (about €42/mo),
`b3-8.consumption` €0.0576/h, `c3-4.consumption` €0.0480/h; monthly
figures not listed in the catalog. Object Storage price not read.
Terraform: `ovh/ovh` together with the OpenStack provider
(**UNVERIFIED** in detail).

## IONOS

IONOS Cloud (the API product, not the IONOS VPS web product): prices
page (<https://cloud.ionos.com/prices>) shown in USD: Basic Cube S (2
vCPU/4 GB) $0.014/h, $10.08 per 30 days; Basic Cube M (4 vCPU/8 GB)
$0.026/h, $18.72 per 30 days; vCPU servers 2 vCPU/8 GB $0.044/h, 4
vCPU/16 GB $0.087/h; S3 Object Storage $0.00487 per GB per 30 days;
outgoing traffic first 2 TB free, then $0.036/GB for the next 8 TB
declining to $0.018/GB above 150 TB; data centres "in the European
Union and in Newark, New Jersey" (locations not itemised on that
page). Terraform `ionos-cloud/ionoscloud` official; the
`ionoscloud_server` resource
(<https://raw.githubusercontent.com/ionos-cloud/terraform-provider-ionoscloud/master/docs/resources/server.md>)
has `ssh_key_path`/`ssh_keys` ("public SSH key that will be injected
into IONOS CLOUD provided Linux images"), `user_data` (initialisation
script), and server `type` ENTERPRISE, CUBE (with `template_uuid`,
mutually exclusive with cores/ram/volume size) or VCPU. REST API at
api.ionos.com/cloudapi/v6 (**UNVERIFIED**: not fetched).

IONOS VPS web line (<https://www.ionos.de/vps>, from €2/mo): no API or
Terraform support found.

## Hexabyte

No European VPS provider by that name was found. hexabyte.tn is a
Tunisian ISP offering fibre/DSL access only (<https://hexabyte.tn>).
hexabyte.com and hexabyte.io present certificate mismatches.
hexabyte.net is a parked domain.

## Hetzner

The cloud page (<https://www.hetzner.com/cloud/>) and Object Storage
page (<https://www.hetzner.com/storage/object-storage/>) render prices
with JavaScript. Readable text confirms locations Falkenstein,
Nuremberg, Helsinki. Object Storage is S3-compatible with a base price
that "includes 1 TB of storage (up to 744 TB-hours) and 1 TB of egress
traffic", free ingress, free internal traffic within eu-central, free
S3 API calls, up to 100 buckets of 100 TB; the base price amount was
not readable (**UNVERIFIED**: the first survey's €5/mo figure from an
aggregator).

## Comparison table

| Provider / line | 2/4 price | 4/8 price | Hourly | API create/destroy | Terraform | S3 price | Egress | EU locations |
|---|---|---|---|---|---|---|---|---|
| OVH VPS | VPS-1 €3.81/mo | Not read | No (monthly only) | **UNVERIFIED** | **UNVERIFIED** (VPS resource on `ovh/ovh` not confirmed) | n/a (not part of VPS line) | Not stated | BHS, DE, GRA, SBG, SGP, SYD, UK, WAW |
| OVH Public Cloud | `d2-4.consumption` €0.0288/h (about €21/mo); mapping to vCPU/RAM not stated in catalog | `d2-8.consumption` €0.0576/h (about €42/mo); `b3-8.consumption` €0.0576/h; `c3-4.consumption` €0.0480/h; mapping not stated | Yes, from 60 seconds | Yes (OpenStack API, cloud-init) | `ovh/ovh` with OpenStack provider (**UNVERIFIED** in detail) | Not read | Not stated | Not stated in this note |
| IONOS Cloud Cubes | Basic Cube S $0.014/h, $10.08/30 days | Basic Cube M $0.026/h, $18.72/30 days | Yes | **UNVERIFIED** (REST API at api.ionos.com/cloudapi/v6 not fetched) | Yes, `ionos-cloud/ionoscloud` official | $0.00487/GB per 30 days | First 2 TB free, then $0.036/GB (next 8 TB), declining to $0.018/GB above 150 TB | "European Union and Newark, New Jersey" (not itemised) |
| IONOS VPS | From €2/mo (not broken down by spec) | From €2/mo (not broken down by spec) | Not stated | None found | None found | n/a | n/a | Not stated |
| Hetzner Cloud | CX23 €3.99/mo or CPX22 €7.99/mo (CAX11 arm64 €4.49/mo) | CX33 €6.49/mo or CPX32 €13.99/mo (CAX21 arm64 €7.99/mo) | Yes | Yes | Yes, `hetznercloud/hcloud` official | Base price amount not readable this round; first survey's €5/mo per TB (**UNVERIFIED**) | Base price includes 1 TB egress; free ingress; free intra-eu-central traffic | Falkenstein, Nuremberg, Helsinki |
| Hexabyte | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a |

## Against the requirements

OVH VPS misses hourly billing, and API ordering and Terraform coverage
are **UNVERIFIED**. OVH Public Cloud misses nothing stated, but its
price is about five times Hetzner's class. IONOS Cloud misses nothing
stated, but its price is about 2.5 times Hetzner's class and is shown
in USD. IONOS VPS misses an API and Terraform support. Hexabyte does
not exist as a VPS provider.
