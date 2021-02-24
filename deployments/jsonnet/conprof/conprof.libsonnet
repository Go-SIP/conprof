local k3 = import 'ksonnet/ksonnet.beta.3/k.libsonnet';
local k = import 'ksonnet/ksonnet.beta.4/k.libsonnet';

{
  local conprof = self,

  config:: {
    name: error 'must provide name',
    namespace: error 'must provide namespace',
    image: error 'must provide image',
    version: error 'must set version',
    namespaces: [conprof.config.namespace],

    commonLabels:: {
      'app.kubernetes.io/name': 'conprof',
      'app.kubernetes.io/instance': conprof.config.name,
      'app.kubernetes.io/version': conprof.config.version,
    },

    podLabelSelector:: {
      [labelName]: conprof.config.commonLabels[labelName]
      for labelName in std.objectFields(conprof.config.commonLabels)
      if !std.setMember(labelName, ['app.kubernetes.io/version'])
    },

    rawconfig:: {
      scrape_configs: [{
        job_name: 'conprof',
        kubernetes_sd_configs: [{
          namespaces: { names: conprof.config.namespaces },
          role: 'pod',
        }],
        relabel_configs: [
          {
            action: 'keep',
            regex: 'conprof.*',
            source_labels: ['__meta_kubernetes_pod_name'],
          },
          {
            source_labels: ['__meta_kubernetes_namespace'],
            target_label: 'namespace',
          },
          {
            source_labels: ['__meta_kubernetes_pod_name'],
            target_label: 'pod',
          },
          {
            source_labels: ['__meta_kubernetes_pod_container_name'],
            target_label: 'container',
          },
        ],
        scrape_interval: '1m',
        scrape_timeout: '1m',
      }],
    },
  },

  roleBindings:
    local roleBinding = k.rbac.v1.roleBinding;

    local newSpecificRoleBinding(namespace) =
      roleBinding.new() +
      roleBinding.mixin.metadata.withName(conprof.config.name) +
      roleBinding.mixin.metadata.withNamespace(namespace) +
      roleBinding.mixin.metadata.withLabels(conprof.config.commonLabels) +
      roleBinding.mixin.roleRef.withApiGroup('rbac.authorization.k8s.io') +
      roleBinding.mixin.roleRef.withName(conprof.config.name) +
      roleBinding.mixin.roleRef.mixinInstance({ kind: 'Role' }) +
      roleBinding.withSubjects([{ kind: 'ServiceAccount', name: conprof.config.name, namespace: conprof.config.namespace }]);

    local roleBindingList = k3.rbac.v1.roleBindingList;
    roleBindingList.new([newSpecificRoleBinding(x) for x in conprof.config.namespaces]),
  roles:
    local role = k.rbac.v1.role;
    local policyRule = role.rulesType;
    local coreRule = policyRule.new() +
                     policyRule.withApiGroups(['']) +
                     policyRule.withResources([
                       'services',
                       'endpoints',
                       'pods',
                     ]) +
                     policyRule.withVerbs(['get', 'list', 'watch']);

    local newSpecificRole(namespace) =
      role.new() +
      role.mixin.metadata.withName(conprof.config.name) +
      role.mixin.metadata.withNamespace(namespace) +
      role.mixin.metadata.withLabels(conprof.config.commonLabels) +
      role.withRules(coreRule);

    local roleList = k3.rbac.v1.roleList;
    roleList.new([newSpecificRole(x) for x in conprof.config.namespaces]),
  secret:
    local secret = k.core.v1.secret;
    secret.new('conprof-config', {}).withStringData({
      'conprof.yaml': std.manifestYamlDoc(conprof.config.rawconfig),
    }) +
    secret.mixin.metadata.withNamespace(conprof.config.namespace) +
    secret.mixin.metadata.withLabels(conprof.config.commonLabels),
  statefulset:
    local statefulset = k.apps.v1.statefulSet;
    local container = statefulset.mixin.spec.template.spec.containersType;
    local volume = statefulset.mixin.spec.template.spec.volumesType;
    local containerPort = container.portsType;
    local containerVolumeMount = container.volumeMountsType;
    local podSelector = statefulset.mixin.spec.template.spec.selectorType;

    local c = [
      container.new('conprof', conprof.config.image) +
      container.withArgs([
        'all',
        '--storage.tsdb.path=/conprof',
        '--config.file=/etc/conprof/conprof.yaml',
      ]) +
      container.withPorts([{ name: 'http', containerPort: 10902 }]) +
      container.withVolumeMounts([
        containerVolumeMount.new('storage', '/conprof'),
        containerVolumeMount.new('config', '/etc/conprof'),
      ],),
    ];

    { apiVersion: 'apps/v1', kind: 'StatefulSet' } +
    statefulset.mixin.metadata.withName(conprof.config.name) +
    statefulset.mixin.metadata.withNamespace(conprof.config.namespace) +
    statefulset.mixin.metadata.withLabels(conprof.config.commonLabels) +
    statefulset.mixin.spec.withPodManagementPolicy('Parallel') +
    statefulset.mixin.spec.withServiceName(conprof.config.name) +
    statefulset.mixin.spec.selector.withMatchLabels(conprof.config.podLabelSelector) +
    statefulset.mixin.spec.template.metadata.withLabels(conprof.config.commonLabels) +
    statefulset.mixin.spec.template.spec.withContainers(c) +
    statefulset.mixin.spec.template.spec.withNodeSelector({ 'kubernetes.io/os': 'linux' }) +
    statefulset.mixin.spec.template.spec.withVolumes([
      volume.fromEmptyDir('storage'),
      volume.fromSecret('config', 'conprof-config'),
    ]) +
    statefulset.mixin.spec.template.spec.withServiceAccountName(conprof.config.name),

  serviceAccount:
    local serviceAccount = k.core.v1.serviceAccount;

    serviceAccount.new(conprof.config.name) +
    serviceAccount.mixin.metadata.withNamespace(conprof.config.namespace) +
    serviceAccount.mixin.metadata.withLabels(conprof.config.commonLabels),

  service:
    local service = k.core.v1.service;
    local servicePort = service.mixin.spec.portsType;

    local httpPort = servicePort.newNamed('http', 10902, 'http');

    service.new(conprof.config.name, conprof.config.podLabelSelector, [httpPort]) +
    service.mixin.metadata.withNamespace(conprof.config.namespace) +
    service.mixin.metadata.withLabels(conprof.config.commonLabels) +
    service.mixin.spec.withClusterIp('None'),

  withServiceMonitor:: {
    local conprof = self,
    serviceMonitor: {
      apiVersion: 'monitoring.coreos.com/v1',
      kind: 'ServiceMonitor',
      metadata: {
        name: 'conprof',
        namespace: conprof.config.namespace,
        labels: conprof.config.commonLabels,
      },
      spec: {
        selector: {
          matchLabels: conprof.config.podLabelSelector,
        },
        endpoints: [
          {
            port: 'http',
            interval: '30s',
          },
        ],
      },
    },
  },

  withConfigMap:: {
    local conprof = self,

    configmap:
      local configmap = k.core.v1.configMap;
      configmap.new('conprof-config', {}).withData({
        'conprof.yaml': std.manifestYamlDoc(conprof.config.rawconfig),
      }) +
      configmap.mixin.metadata.withNamespace(conprof.config.namespace) +
      configmap.mixin.metadata.withLabels(conprof.config.commonLabels),

    statefulset+: {
      spec+: {
        template+: {
          spec+: {
            volumes:
              std.map(
                function(v) if v.name == 'config' then v {
                  secret:: null,
                  configMap: {
                    name: conprof.configmap.metadata.name,
                  },
                } else v,
                super.volumes
              ),
          },
        },
      },
    },

    secret:: null,
  },
}
