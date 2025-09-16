# Advertise Addresses in Armada

## Účel Advertise Addresses (Purpose of Advertise Addresses)

Advertise addresses v Armada slouží k řešení NAT traversal problémů a umožňují správnou komunikaci mezi uzly clusteru a klienty v síťových prostředích s Network Address Translation (NAT) nebo firewally.

**English**: Advertise addresses in Armada serve to solve NAT traversal problems and enable proper communication between cluster nodes and clients in network environments with Network Address Translation (NAT) or firewalls.

## Typy Advertise Addresses (Types of Advertise Addresses)

### 1. API Advertise Address (`api.advertise-address`)

**Účel**: Veřejná adresa API serveru, kterou používají klienti pro připojení k Armada instanci.

**Purpose**: Public address of the API server used by clients to connect to the Armada instance.

- **Výchozí hodnota**: `http://127.0.0.1:8443`
- **Použití**: Klienti používají tuto adresu pro gRPC/HTTP komunikaci s Armada serverem
- **NAT traversal**: Když Armada běží za NAT nebo load balancerem, tato adresa umožňuje klientům najít správný endpoint

**Příklad konfigurace**:
```bash
# Místní vývoj
--api.advertise-address="http://127.0.0.1:8443"

# Produkční nasazení za load balancerem  
--api.advertise-address="https://armada.company.com:443"

# Specifická IP adresa
--api.advertise-address="http://192.168.1.100:8443"
```

### 2. Memberlist Advertise Address (`memberlist.advertise-address`)

**Účel**: Adresa pro gossip protokol mezi Armada instancemi v clusteru.

**Purpose**: Address for gossip protocol between Armada instances in the cluster.

- **Výchozí hodnota**: prázdná (používá se bind address)
- **Použití**: Gossip služby na vzdálených Armada instancech používají tuto adresu pro výměnu zpráv
- **Cluster discovery**: Umožňuje automatické objevování uzlů v clusteru

**Příklad konfigurace**:
```bash
# Automatické použití bind address (doporučeno pro jednoduché sítě)
--memberlist.advertise-address=""

# Explicitní adresa pro NAT prostředí
--memberlist.advertise-address="10.0.1.50:7432"

# Veřejná IP pro cloud nasazení
--memberlist.advertise-address="203.0.113.10:7432"
```

## Scénáře použití (Usage Scenarios)

### Scénář 1: Lokální vývoj
```bash
./armada leader \
  --api.address="http://0.0.0.0:8443" \
  --api.advertise-address="http://127.0.0.1:8443" \
  --memberlist.address="0.0.0.0:7432" \
  --memberlist.advertise-address=""
```

### Scénář 2: Docker kontejner
```bash
# Kontejner běží s porty mapovanými na host
./armada leader \
  --api.address="http://0.0.0.0:8443" \
  --api.advertise-address="http://192.168.1.100:8443" \
  --memberlist.address="0.0.0.0:7432" \
  --memberlist.advertise-address="192.168.1.100:7432"
```

### Scénář 3: Cloud deployment s load balancerem
```bash
./armada leader \
  --api.address="http://0.0.0.0:8443" \
  --api.advertise-address="https://armada-api.company.com:443" \
  --memberlist.address="0.0.0.0:7432" \
  --memberlist.advertise-address="10.0.1.50:7432"
```

### Scénář 4: Multi-region cluster
```bash
# Region US-East
./armada leader \
  --api.advertise-address="https://armada-us-east.company.com:8443" \
  --memberlist.advertise-address="203.0.113.10:7432" \
  --memberlist.members="203.0.113.20:7432,203.0.113.30:7432"

# Region EU-West  
./armada follower \
  --api.advertise-address="https://armada-eu-west.company.com:8443" \
  --memberlist.advertise-address="203.0.113.20:7432" \
  --memberlist.members="203.0.113.10:7432,203.0.113.30:7432"
```

## Rozdíl mezi Listen a Advertise adresami

### Listen Address
- Adresa, na které služba **naslouchá** příchozím spojením
- Obvykle `0.0.0.0:port` pro naslouchání na všech interfacech
- Interní síťová konfigurace

### Advertise Address  
- Adresa, kterou služba **oznamuje** ostatním jako svou veřejnou adresu
- Adresa, kterou by měli ostatní používat pro připojení
- Řešení pro NAT/firewall/proxy situace

## Příklad s NAT

```
Internet                NAT Gateway              Private Network
   |                         |                        |
Client  ────────────────> [Router] ──────────────> Armada Server
                         203.0.113.10              192.168.1.100
                         :8443                     :8443
```

**Konfigurace serveru**:
```bash
--api.address="http://0.0.0.0:8443"           # Naslouchá na všech interfacech
--api.advertise-address="http://203.0.113.10:8443"  # Klienti používají veřejnou IP
```

**Konfigurace klienta**:
```bash
# Klient se připojuje k advertise address
armada-client --endpoint="http://203.0.113.10:8443"
```

## Důležité poznámky (Important Notes)

1. **API advertise address** musí být dostupný pro všechny klienty
2. **Memberlist advertise address** musí být dostupný pro všechny uzly clusteru
3. Pokud není advertise address nastaven, používá se listen address
4. Pro HTTPS/TLS nasazení nezapomeňte na správné certifikáty pro advertise address
5. DNS jména v advertise address musí být resolvable ze všech relevantních míst

## Další dokumentace (Additional Documentation)

- **[Configuration Examples](advertise-addresses-examples.toml)** - Practical configuration examples for different deployment scenarios
- **[Troubleshooting Guide](advertise-addresses-troubleshooting.md)** - Common problems and solutions for advertise address issues

## Řešení problémů (Troubleshooting)

### Symptom: Klienti se nemohou připojit
- Zkontrolujte, zda je `api.advertise-address` dostupný z klientské sítě
- Ověřte firewall pravidla pro advertise port
- Otestujte dostupnost: `telnet <advertise-host> <advertise-port>`

### Symptom: Uzly clusteru se nemohou najít
- Zkontrolujte `memberlist.advertise-address` dostupnost mezi uzly
- Ověřte memberlist porty (UDP i TCP)
- Zkontrolujte DNS rozlišení hostnames

### Symptom: Nekonzistentní cluster stav
- Ujistěte se, že všechny uzly používají stejný `memberlist.cluster-name`
- Ověřte, že advertise adresy jsou skutečně dostupné z ostatních uzlů
- Zkontrolujte síťové latence mezi uzly