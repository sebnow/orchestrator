# VPS providers for daemons and the server: survey (2026-10-10)

The owner's requirements: cheap Linux VPSes to run daemons (CPU and
RAM, no GPU, x86_64 or arm64), a provisioning API, a Terraform
provider, European data centres, S3-compatible object storage from the
same provider (to back up the server's SQLite database and journals),
and hourly billing preferred; macOS is not a VPS target. This note
records a web survey done by an agent on 2026-10-10.

Several prices below come from third-party aggregator or blog pages
rather than the provider's own pricing page, named per provider:
Hetzner's figures are also cross-checked against
<https://agentdeals.dev/hetzner-pricing-2026> (an aggregator);
Infomaniak's figures come from
<https://infoswitch.fr/en/blog/infomaniak-pricing-2026-all-services>
(a blog); netcup's figures come from
<https://valebyte.com/en/blog/netcup-vs-hetzner-vs-valebyte-comparison-of-budget-vps-2026/>
(a competitor's blog). All of these, and anything marked
**UNVERIFIED**, must be confirmed on the provider's own pricing page
before any decision is acted on.

## Hetzner Cloud

2 vCPU/4 GB: CX23 at €3.99/mo or CPX22 at €7.99/mo; CAX11 (arm64) at
€4.49/mo. 4 vCPU/8 GB: CX33 at €6.49/mo or CPX32 at €13.99/mo; CAX21
(arm64) at €7.99/mo. Hourly billing is available, IPv4 costs €0.50/mo,
and traffic is included. Data centres are in Frankfurt, Helsinki, and
Nuremberg. The provisioning API is a REST API
(<https://docs.hetzner.cloud/>) supporting cloud-init and SSH keys,
with servers ready in seconds to minutes. The Terraform provider is
`hetznercloud/hcloud`, official
(<https://github.com/hetznercloud/terraform-provider-hcloud>). Object
Storage is S3-compatible, €5/mo per TB, with egress at €1/TB (intra-EU
free), in an EU region. Hetzner is German-owned. Prices via
<https://agentdeals.dev/hetzner-pricing-2026> (aggregator) and
<https://docs.hetzner.cloud/reference/cloud>.

## Scaleway

2 vCPU/4 GB: VPS-STORE-2-S at €16.49/mo. 4 vCPU/8 GB: VPS-PRO-2-M at
€17.49/mo (125 GB SSD) or VPS-START-2-L at €23.49/mo. x86 only. Hourly
billing, traffic included. Data centres are in Paris and Amsterdam.
The provisioning API is a REST API
(<https://scaleway.com/en/docs/>). The Terraform provider is
`scaleway/scaleway`, official
(<https://scaleway.com/en/docs/terraform/quickstart>). Object Storage
Standard One Zone is €0.00752/GB-month, with egress at €0.01/GB after
75 GB free per month, in Paris or Amsterdam, and AWS SDK compatible
(<https://www.scaleway.com/en/pricing/storage>). Scaleway is
French-owned.

## OVHcloud

**UNVERIFIED** ~$5.85/mo (2 vCPU/4 GB) and ~$10/mo (4 vCPU/8 GB), in
USD. x86 only. Data centres are in Strasbourg, Gravelines, and
Roubaix. The provisioning API is REST
(<https://eu.api.ovh.com/v1/>); whether the VPS tier supports
cloud-init is **UNVERIFIED**. The Terraform provider is
`ovhcloud/ovh`, official, but its VPS coverage is **UNVERIFIED** — it
centres on Public Cloud. Object Storage starts from $2/mo, around
$0.0081/GB-month, with no egress fees except to Asia-Pacific, and is
S3-compatible. OVHcloud is French-owned.

## UpCloud

$30/mo (2 vCPU/4 GB, hourly $0.0489) and $70/mo (4 vCPU/8 GB, hourly
$0.0883); EUR pricing is **UNVERIFIED**. x86 only. Data centres are in
Helsinki, London, and Amsterdam. The provisioning API is REST
(<https://upcloud.com/api/>). The Terraform provider is
`UpCloudLtd/upcloud`, official. Managed Object Storage is
S3-compatible; pricing was not found
(<https://upcloud.com/docs/tutorials/upcloud-object-storage-terraform/>).
UpCloud is Finnish-owned.

## Exoscale

Pricing for 2 vCPU/4 GB was not found. 4 vCPU/8 GB: Large at
€67.20/mo, **UNVERIFIED**. Data centres are in Zurich and Geneva. The
provisioning API is the CloudStack REST API
(<https://community.exoscale.com>). The Terraform provider is
`exoscale/exoscale`, official. Object Storage is €0.018/GB-month, with
egress at €0.01818/GB, in Zurich, and S3-compatible. Exoscale is
Swiss-owned.

## Infomaniak

2 vCPU/4 GB: VPS Lite M at €9.90/mo or VPS Cloud M at €18/mo. Pricing
for 4 vCPU/8 GB was not found. Data centres are in Zurich and Geneva.
The provisioning API is an OpenStack REST API; cloud-init support is
**UNVERIFIED**, as is hourly billing. The Terraform provider is
`infomaniak/infomaniak`, official. Object Storage (Swift,
S3-compatible) is €0.010/GB-month, in Switzerland. Infomaniak is
Swiss-owned. Prices via
<https://infoswitch.fr/en/blog/infomaniak-pricing-2026-all-services>
(blog).

## Contabo

2 vCPU/4 GB: Storage VPS 10 at €4.50/mo (300 GB SSD). 4 vCPU/8 GB:
Cloud VPS 10 at €4.50/mo (75 GB NVMe), or Cloud VPS 4 at €5.28 to
€6.60/mo. x86 only. Hourly billing with flexible terms, unlimited
traffic under fair use. Data centres are in Germany and the
Netherlands. The provisioning API is REST
(<https://api.contabo.com/>, OpenAPI, OAuth2, cloud-init, SSH keys).
The Terraform provider is `contabo/terraform-provider-contabo`,
official
(<https://github.com/contabo/terraform-provider-contabo>,
<https://contabo.com/blog/terraform-contabo-vps/>). Object Storage is
€2.49/mo per 250 GB, unlimited traffic, S3-compatible; the EU region is
not stated explicitly
(<https://contabo.com/object-storage>). Contabo is German-owned.

## netcup

2 vCPU/4 GB: VPS 500 G12 at €5.91/mo. 4 vCPU/8 GB: VPS 1000 G12 at
€10.37/mo. Hourly billing, traffic included. Data centres are in
Germany. The SCP REST API manages existing servers only; VPS creation
needs a manual checkout. The Terraform provider is
`rincedd/netcup-scp`, community, and cannot create servers
(<https://github.com/rincedd/terraform-provider-netcup-scp>). No
S3-compatible storage was found. netcup is German-owned. Prices via
<https://valebyte.com/en/blog/netcup-vs-hetzner-vs-valebyte-comparison-of-budget-vps-2026/>
(a competitor's blog). netcup fails the provisioning requirement.

## DigitalOcean

2 vCPU/4 GB: $18 to $24/mo (€12 to €22, **UNVERIFIED**). Pricing for 4
vCPU/8 GB is not separated. x86 only. Hourly billing, egress $0.01/GB
beyond 2 TB. Data centres are in London, Frankfurt, and Amsterdam. The
provisioning API is REST
(<https://docs.digitalocean.com/reference/api/>). The Terraform
provider is `digitalocean/digitalocean`, official. Spaces is
S3-compatible; pricing was not found; the region is EU. DigitalOcean
is US-owned.

## Vultr

2 vCPU/4 GB: $24/mo (€20 to €22, **UNVERIFIED**). x86 only. Data
centres are in London, Amsterdam, Paris, and Frankfurt. The
provisioning API is REST (<https://docs.vultr.com/reference>). The
Terraform provider is `vultr/vultr`, official. Object Storage is
S3-compatible; pricing was not found; 3 TB/month of egress is free.
Vultr is US-owned.

## Linode/Akamai

2 vCPU/4 GB: $24/mo (hourly $0.036). x86 only. Data centres are in
Stockholm, Frankfurt, Paris, Amsterdam, and London. The provisioning
API is REST (<https://techdocs.akamai.com/linode-api/>). The Terraform
provider is `linode/linode`, official. Object Storage is
S3-compatible; pricing was not found. Linode is an Akamai subsidiary.

## Comparison table

| Provider | 2 vCPU/4 GB | 4 vCPU/8 GB | arm64 | REST API | Official Terraform | S3 price | Egress | EU-owned |
|---|---|---|---|---|---|---|---|---|
| Hetzner Cloud | CX23 €3.99/mo or CPX22 €7.99/mo (CAX11 €4.49/mo) | CX33 €6.49/mo or CPX32 €13.99/mo (CAX21 €7.99/mo) | Yes | Yes | Yes | €5/mo per TB | €1/TB, intra-EU free | Yes (German) |
| Scaleway | VPS-STORE-2-S €16.49/mo | VPS-PRO-2-M €17.49/mo or VPS-START-2-L €23.49/mo | No | Yes | Yes | €0.00752/GB-month | €0.01/GB after 75 GB free | Yes (French) |
| OVHcloud | **UNVERIFIED** ~$5.85/mo | **UNVERIFIED** ~$10/mo | No | Yes | Official; VPS coverage **UNVERIFIED** | ~$0.0081/GB-month | None except Asia-Pacific | Yes (French) |
| UpCloud | $30/mo | $70/mo | No | Yes | Yes | Not found | Not found | Yes (Finnish) |
| Exoscale | Not found | €67.20/mo **UNVERIFIED** | No | Yes (CloudStack) | Yes | €0.018/GB-month | €0.01818/GB | Yes (Swiss) |
| Infomaniak | VPS Lite M €9.90/mo or VPS Cloud M €18/mo | Not found | No | Yes (OpenStack) | Yes | €0.010/GB-month | Not stated | Yes (Swiss) |
| Contabo | Storage VPS 10 €4.50/mo | Cloud VPS 10 €4.50/mo or Cloud VPS 4 €5.28-6.60/mo | No | Yes | Yes | €2.49/mo per 250 GB | Unlimited (fair use) | Yes (German) |
| netcup | VPS 500 G12 €5.91/mo | VPS 1000 G12 €10.37/mo | No | Manage-only, no creation | Community; cannot create servers | Not found | - | Yes (German) |
| DigitalOcean | $18 to $24/mo (€12 to €22 **UNVERIFIED**) | Not separated | No | Yes | Yes | Not found | $0.01/GB beyond 2 TB | No (US) |
| Vultr | $24/mo (€20 to €22 **UNVERIFIED**) | - | No | Yes | Yes | Not found | 3 TB/month free | No (US) |
| Linode/Akamai | $24/mo | - | No | Yes | Yes | Not found | - | No (US, Akamai) |

## Against the requirements

Hetzner meets every stated requirement: a provisioning API, an
official Terraform provider, EU data centres, S3-compatible storage, a
low price, arm64 instances, and hourly billing. Scaleway meets all but
price and arm64. Contabo meets all but arm64 and an unstated storage
region. netcup fails the provisioning requirement outright. The US
providers (DigitalOcean, Vultr, Linode/Akamai) fail EU ownership. The
rest — OVHcloud, UpCloud, Exoscale, Infomaniak — have pricing or
coverage gaps that leave them unconfirmed against the requirements.

This note makes no recommendation.
