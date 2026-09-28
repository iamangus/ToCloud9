# ToCloud9 Helm chart

This chart will deploy a ToCloud9 cluster using **AzerothCore** as backend.

## Databases

By default this chart deploy a really simple mysql dependency chart and will create the required user and databases. However, it won't create the required tables. You need to create them with the [AzerothCore sql scripts](https://github.com/azerothcore/azerothcore-wotlk/tree/master/data/sql/base) and [ToCloud9 modifications](../doc/RunNonDockerWinWSLAzerothCore.md#setup--run).

### BYOD (Brin Your Own Database)

You can also use a preexisting database by setting the `databases.external_db_hostname` variable.

## How to use

1. Check the [values](values.yaml) to set a storage class for persistency and game-load-balancer domain.

2. Deploy the chart

3. Most services won't start as long as the databases tables aren't initialized. See Databases for more details.

4. Gameserver will require its `/data` volume to be provisionned with files. Do this, you can :
  - Use the gameserver init container to download the required file if your PV is empty. Check `gameserver_ac.initcontainer.download_url` for zip download origin.
  - Provision the files on your own once the PV is created, if `gameserver_ac.initcontainer.download_enabled` is disabled.

### Example

Here is an example of how to install the chart with an external database and the `my_domain_or_ip.com` domain.
Make sure that the `gateway` service is reachable at the specified address.

```
cd $TO_CLOUD9_PROJECT_ROOT_DIR
kubectl create ns tocloud9
helm install cloud9 ./chart -n tocloud9 \
   --set gameserver_ac.storageClass=standard \
   --set mysql.enabled=false \
   --set databases.db_user=db_user \
   --set databases.db_password=db_pass \
   --set databases.external_db_hostname=192.168.65.2 \
   --set gateway.hostname=my_domain_or_ip.com \
   --set gateway.service.type=ClusterIP
```

More parameters specified in the [values](values.yaml) file.

## External playerbot agent controller

The `agent_controller` StatefulSet is disabled by default. Enable both
`agent_controller.enabled` and `playerbots.agentBridge.enabled`. It connects to ToCloud9
NATS and Redis, consumes assigned bot-event shards, and routes typed primitive
commands to the owning worldserver using the event's owner token. The
`iamangus/mod-playerbots` GitHub Actions workflow builds and publishes
`ghcr.io/iamangus/playerbot-agent-controller` from `mod-playerbots/agent-controller`.
Make that GHCR package public for unauthenticated cluster pulls, or configure an image
pull secret. Then provide an OpenAI-compatible tool-calling endpoint/model.
Provide the LLM API key through an existing Kubernetes Secret (`api-key` key), not a
plain values file. The controller stores persona, memory, and external task state in
Redis; it does not connect to the AzerothCore databases.

Create the secret in the release namespace, then set these values in your normal
ToCloud9 values override:

```yaml
playerbots:
  enabled: true
  agentBridge:
    enabled: true
    botGuid: 0 # 0 enables every playerbot; use one GUID counter for a canary
agent_controller:
  enabled: true
  replicaCount: 4
  image:
    repository: ghcr.io/iamangus/playerbot-agent-controller
    tag: latest
  apiKeySecretName: playerbot-llm
  modelEndpoint: https://<provider>/v1/chat/completions
  modelName: <tool-capable-model>
```

The controller partitions bots across 256 virtual event shards. Each StatefulSet pod
subscribes to its assigned shards; increasing `agent_controller.replicaCount` moves
shards between pods. The chart enables file-backed JetStream with a bounded event
stream, Redis actor leases fence overlapping ownership during resharding, and bot
heartbeats allow state/snapshot recovery after a rollout.

The playerbot module must be compiled into the deployed gameserver image with
ToCloud9's `libsidecar` available at build and runtime. The module publishes to
`playerbots.v1.events.<shard>.<owner>.<bot>` and subscribes to
`playerbots.v1.commands.<owner>`. Keep the worldserver gRPC/NATS endpoints internal
to the cluster.

The stock ToCloud9 gameserver image does not include `mod-playerbots`. Build a
custom gameserver image from a core revision compatible with this module, while
retaining the ToCloud9 patches and sidecar. In this public checkout, the
gameserver Dockerfile references `game-server/azerothcore/patches`, which is not
present in the repository tree; reconcile that build context with the source used
by your deployed image before building.
