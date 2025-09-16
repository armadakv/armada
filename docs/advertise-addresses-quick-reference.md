# Advertise Addresses in Armada - Quick Reference

## Co to je (What it is)

Advertise addresses are public network addresses that Armada nodes announce to clients and other cluster members to enable proper connectivity in complex network environments like NAT, firewalls, load balancers, and multi-region deployments.

## Dva hlavní typy (Two main types)

| Type | Flag | Purpose | Used by |
|------|------|---------|---------|
| **API Advertise Address** | `--api.advertise-address` | Client connections to API server | gRPC/HTTP clients |
| **Memberlist Advertise Address** | `--memberlist.advertise-address` | Inter-node gossip communication | Other Armada nodes in cluster |

## Kdy je potřebujete (When you need them)

✅ **You NEED advertise addresses when:**
- Running behind NAT/firewall
- Using Docker with port mapping
- Deploying behind load balancer
- Multi-region cluster setup
- Cloud deployments (AWS, GCP, Azure)
- Kubernetes deployments

❌ **You DON'T need advertise addresses when:**
- Simple local development
- All nodes on same flat network
- No NAT/firewall/proxy involved

## Rychlá konfigurace (Quick configuration)

### Local Development
```bash
# Usually defaults work fine
./armada leader
```

### Docker
```bash
./armada leader \
  --api.advertise-address="http://192.168.1.100:8443" \
  --memberlist.advertise-address="192.168.1.100:7432"
```

### Production Cloud
```bash
./armada leader \
  --api.advertise-address="https://armada-api.company.com:443" \
  --memberlist.advertise-address="10.0.1.50:7432"
```

## Rychlá diagnostika (Quick diagnosis)

### Test API connectivity
```bash
curl -v http://YOUR-API-ADVERTISE-ADDRESS/health
```

### Test Memberlist connectivity
```bash
nc -zv YOUR-MEMBERLIST-ADVERTISE-HOST 7432
```

### Check cluster members
```bash
curl http://localhost:8079/v1/maintenance/memberlist/members
```

## Často řešené problémy (Common problems)

| Problem | Quick Fix |
|---------|-----------|
| Clients can't connect | Check API advertise address is reachable from client |
| Nodes can't find each other | Verify memberlist advertise addresses and ports |
| Works locally, fails in Docker | Use host IP in advertise addresses |
| Kubernetes issues | Use service DNS names |

## Další zdroje (More resources)

- 📖 [Complete Guide](advertise-addresses.md) - Detailed explanation
- ⚙️ [Configuration Examples](advertise-addresses-examples.toml) - Ready-to-use configs  
- 🔧 [Troubleshooting Guide](advertise-addresses-troubleshooting.md) - Problem solving

## Shrnutí pro začátečníky (Summary for beginners)

**Listen address** = where Armada listens for connections (usually `0.0.0.0:port`)  
**Advertise address** = what Armada tells others to use to connect to it

Think of it like this:
- Your house (server) is on Main Street 123 (listen address)
- But your mailing address for others is P.O. Box 456 (advertise address)
- People send mail to P.O. Box 456, which gets delivered to Main Street 123