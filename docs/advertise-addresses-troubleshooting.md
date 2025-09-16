# Troubleshooting Advertise Addresses in Armada

This guide helps diagnose and fix common issues related to advertise address configuration in Armada.

## Quick Diagnosis Commands

### Test API Advertise Address Connectivity
```bash
# Test if API advertise address is reachable
curl -v http://<api-advertise-address>/health

# Test gRPC connectivity  
grpcurl -plaintext <api-advertise-address> list

# Test with telnet
telnet <api-advertise-host> <api-advertise-port>
```

### Test Memberlist Advertise Address Connectivity
```bash
# Test TCP connectivity (memberlist uses both TCP and UDP)
nc -zv <memberlist-advertise-host> <memberlist-advertise-port>

# Test UDP connectivity
nc -uzv <memberlist-advertise-host> <memberlist-advertise-port>

# Check if port is listening
netstat -tulpn | grep <memberlist-advertise-port>
```

## Common Problems and Solutions

### Problem 1: Clients Cannot Connect to API

**Symptoms:**
- Connection timeouts to API
- "connection refused" errors
- gRPC connection failures

**Diagnosis:**
```bash
# Check if API server is listening
netstat -tlpn | grep :8443

# Check if advertise address is reachable from client
curl -v http://<api-advertise-address>/health
```

**Solutions:**
1. **Firewall blocking**: Open firewall ports for API advertise address
2. **Wrong advertise address**: Ensure API advertise address is reachable from client network
3. **DNS resolution**: Verify DNS resolution of advertise hostname
4. **Load balancer misconfiguration**: Check load balancer health checks

**Example Fix:**
```bash
# If behind NAT, ensure advertise address uses external IP
--api.address="http://0.0.0.0:8443"
--api.advertise-address="http://203.0.113.10:8443"  # External IP
```

### Problem 2: Cluster Nodes Cannot Discover Each Other

**Symptoms:**
- Single node clusters when multiple nodes are started
- "failed to fast join cluster" warnings
- Empty member lists

**Diagnosis:**
```bash
# Check memberlist port connectivity between nodes
nc -zv <other-node-advertise-ip> 7432

# Check logs for gossip-related errors
grep -i "memberlist\|gossip" /var/log/armada.log
```

**Solutions:**
1. **Network connectivity**: Ensure memberlist ports (TCP+UDP) are open between nodes
2. **Advertise address mismatch**: Verify advertise addresses are reachable from other nodes
3. **Cluster name mismatch**: Ensure all nodes use same `memberlist.cluster-name`

**Example Fix:**
```bash
# Node 1
--memberlist.advertise-address="10.0.1.10:7432"
--memberlist.members="10.0.1.20:7432,10.0.1.30:7432"
--memberlist.cluster-name="production"

# Node 2  
--memberlist.advertise-address="10.0.1.20:7432"
--memberlist.members="10.0.1.10:7432,10.0.1.30:7432"
--memberlist.cluster-name="production"
```

### Problem 3: Docker Container Connectivity Issues

**Symptoms:**
- Clients can't reach API from host machine
- Other containers can't reach Armada

**Diagnosis:**
```bash
# Check Docker port mapping
docker port <container-name>

# Check if container IP is accessible
docker inspect <container-name> | grep IPAddress
```

**Solutions:**
1. **Port mapping**: Ensure Docker ports are properly mapped
2. **Advertise address**: Use host IP in advertise address, not container IP
3. **Docker network**: Use appropriate Docker network for inter-container communication

**Example Fix:**
```bash
# Run container with port mapping
docker run -p 8443:8443 -p 7432:7432 armada:latest \
  --api.address="http://0.0.0.0:8443" \
  --api.advertise-address="http://192.168.1.100:8443" \
  --memberlist.address="0.0.0.0:7432" \
  --memberlist.advertise-address="192.168.1.100:7432"
```

### Problem 4: Cloud/Kubernetes Deployment Issues

**Symptoms:**
- Intermittent connectivity
- Load balancer health check failures
- Service discovery problems

**Diagnosis:**
```bash
# Check Kubernetes service
kubectl get svc armada-service

# Check pod networking
kubectl exec -it <armada-pod> -- netstat -tlpn

# Check service endpoints
kubectl get endpoints armada-service
```

**Solutions:**
1. **Service configuration**: Ensure Kubernetes service targets correct ports
2. **Health checks**: Configure proper health check endpoints
3. **Network policies**: Verify network policies allow required traffic

**Example Fix:**
```yaml
# Kubernetes service
apiVersion: v1
kind: Service
metadata:
  name: armada-service
spec:
  ports:
  - name: api
    port: 8443
    targetPort: 8443
  - name: memberlist
    port: 7432
    targetPort: 7432
  selector:
    app: armada
```

## Advanced Debugging

### Enable Debug Logging
```bash
# Start Armada with verbose logging
./armada leader --dev-mode --log-level=DEBUG
```

### Network Traffic Analysis
```bash
# Monitor traffic on API port
tcpdump -i any port 8443

# Monitor traffic on memberlist port  
tcpdump -i any port 7432
```

### Memberlist Specific Debugging
```bash
# Check memberlist statistics via maintenance API
curl http://localhost:8079/v1/maintenance/memberlist/stats

# List current members
curl http://localhost:8079/v1/maintenance/memberlist/members
```

## Prevention Best Practices

1. **Test connectivity** before production deployment
2. **Use DNS names** instead of IP addresses when possible
3. **Document network topology** for your deployment
4. **Monitor cluster health** with appropriate tooling
5. **Validate advertise addresses** are reachable from all required locations

## Getting Help

If you're still experiencing issues:

1. **Check logs** for specific error messages
2. **Verify network connectivity** between all components  
3. **Test with minimal configuration** first
4. **Ask for help** in [GitHub Discussions](https://github.com/armadakv/armada/discussions)

Include the following information when asking for help:
- Complete configuration used
- Network topology diagram
- Relevant log excerpts
- Output of connectivity tests