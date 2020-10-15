/* Copyright © 2020 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package configmap

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-network-operator/pkg/render"
	"github.com/pkg/errors"
	operatortypes "github.com/vmware/nsx-container-plugin-operator/pkg/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	manifestDir string = "./manifest"
)

func FillDefaults(configmap *corev1.ConfigMap, spec *configv1.NetworkSpec) error {
	errs := []error{}
	data := &configmap.Data
	cfg, err := ini.Load([]byte((*data)[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "failed to load ConfigMap")
		return err
	}
	// We support only policy API, single tier topo on openshift4
	appendErrorIfNotNil(&errs, fillDefault(cfg, "coe", "adaptor", "openshift4", true))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "nsx_v3", "policy_nsxapi", "True", true))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "nsx_v3", "single_tier_topology", "True", true))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "nsx_v3", "wait_for_security_policy_sync", "True", false))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "coe", "enable_snat", "True", false))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "ha", "enable", "True", false))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "k8s", "process_oc_network", "False", true))
	// For Openshift add a 3-seconds agent delay by default
	appendErrorIfNotNil(&errs, fillDefault(cfg, "nsx_node_agent", "waiting_before_cni_response", "3", false))
	appendErrorIfNotNil(&errs, fillDefault(cfg, "nsx_node_agent", "mtu", strconv.Itoa(operatortypes.DefaultMTU), false))
	appendErrorIfNotNil(&errs, fillClusterNetwork(spec, cfg))

	// Write config back to ConfigMap data
	(*data)[operatortypes.ConfigMapDataKey], err = iniWriteToString(cfg)
	appendErrorIfNotNil(&errs, err)

	if len(errs) > 0 {
		return errors.Errorf("failed to fill defaults: %q", errs)
	}
	return nil
}

func appendErrorIfNotNil(errs *[]error, err error) {
	if err == nil {
		return
	}
	*errs = append(*errs, err)
}

func fillDefault(cfg *ini.File, sec string, key string, val string, force bool) error {
	_, err := cfg.GetSection(sec)
	if err != nil {
		return errors.Wrapf(err, "failed to get section %s", sec)
	}
	if !cfg.Section(sec).HasKey(key) {
		_, err = cfg.Section(sec).NewKey(key, val)
		if err != nil {
			return errors.Wrapf(err, "failed to fill key %s default value %s in section %s", key, val, sec)
		}
	} else if cfg.Section(sec).Key(key).Value() == "" || force == true {
		cfg.Section(sec).Key(key).SetValue(val)
	}
	return nil
}

func fillClusterNetwork(spec *configv1.NetworkSpec, cfg *ini.File) error {
	ipBlocks := []string{}
	for _, block := range spec.ClusterNetwork {
		ipBlocks = append(ipBlocks, block.CIDR)
	}
	return fillDefault(cfg, "nsx_v3", "container_ip_blocks", strings.Join(ipBlocks[:], ","), true)
}

func Validate(configmap *corev1.ConfigMap, spec *configv1.NetworkSpec) error {
	errs := []error{}

	errs = append(errs, validateConfigMap(configmap)...)
	errs = append(errs, validateClusterNetwork(spec)...)

	if len(errs) > 0 {
		return errors.Errorf("invalid configuration: %q", errs)
	}
	return nil
}

func validateConfig(cfg *ini.File, sec string, key string) error {
	_, err := cfg.GetSection(sec)
	if err != nil {
		return errors.Wrapf(err, "failed to get section %s", sec)
	}
	if !cfg.Section(sec).HasKey(key) || cfg.Section(sec).Key(key).Value() == "" {
		return errors.Errorf("failed to get key %s from section %s", key, sec)
	}
	return nil
}

func validateConfigMap(configmap *corev1.ConfigMap) []error {
	errs := []error{}
	data := configmap.Data
	cfg, err := ini.Load([]byte(data[operatortypes.ConfigMapDataKey]))
	if err != nil {
		errs = append(errs, errors.Wrapf(err, "failed to load ConfigMap"))
		return errs
	}
	appendErrorIfNotNil(&errs, validateConfig(cfg, "coe", "cluster"))
	appendErrorIfNotNil(&errs, validateConfig(cfg, "nsx_v3", "nsx_api_managers"))
	if cfg.Section("coe").Key("enable_snat").Value() == "True" {
		appendErrorIfNotNil(&errs, validateConfig(cfg, "nsx_v3", "external_ip_pools"))
	}
	// Either T0 gateway or top tier router should be set
	if validateConfig(cfg, "nsx_v3", "tier0_gateway") != nil &&
		validateConfig(cfg, "nsx_v3", "top_tier_router") != nil {
		appendErrorIfNotNil(&errs, errors.Errorf("failed to get tier0_gateway or top_tier_router"))
	}
	// Check MTU value
	mtu := cfg.Section("nsx_node_agent").Key("mtu").Value()
	_, err = strconv.Atoi(mtu)
	if err != nil {
		appendErrorIfNotNil(&errs, errors.Wrapf(err, "mtu is invalid"))
	}

	return errs
}

func getPrefixFromCIDR(cidr string) (uint32, error) {
	cidrPrefix := cidr[strings.LastIndex(cidr, "/")+1:]
	i, err := strconv.ParseUint(cidrPrefix, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(i), nil
}

func validateClusterNetwork(spec *configv1.NetworkSpec) []error {
	errs := []error{}
	if strings.ToLower(spec.NetworkType) != operatortypes.NetworkType {
		appendErrorIfNotNil(&errs, errors.Errorf("network type %s is not %s", spec.NetworkType, operatortypes.NetworkType))
		return errs
	}
	if len(spec.ClusterNetwork) == 0 {
		appendErrorIfNotNil(&errs, errors.Errorf("cluster network cannot be empty"))
		return errs
	}
	for idx, pool := range spec.ClusterNetwork {
		_, _, err := net.ParseCIDR(pool.CIDR)
		if err != nil {
			appendErrorIfNotNil(&errs, errors.Wrapf(err, "cluster network %d CIDR %q is invalid", idx, pool.CIDR))
			continue
		}
		cidrPrefix, err := getPrefixFromCIDR(pool.CIDR)
		if err != nil {
			appendErrorIfNotNil(&errs, errors.Wrapf(err, "unable to infer CIDR prefix in CIDR: %q", pool.CIDR))
			continue
		}

		if cidrPrefix > 30 {
			appendErrorIfNotNil(&errs, errors.Errorf("invalid CIDR prefix length for CIDR %q. it must be larger than 30", pool.CIDR))
		}

		if pool.HostPrefix < cidrPrefix {
			appendErrorIfNotNil(&errs, errors.Errorf("invalid CIDR HostPrefix: %d for CIDR %q. It must be smaller than or equal to the CIDR Prefix", pool.HostPrefix, pool.CIDR))
		}
	}
	return errs
}

func Render(configmap *corev1.ConfigMap, ncpReplicas int32, nsxSecret *corev1.Secret,
	lbSecret *corev1.Secret) ([]*unstructured.Unstructured, error) {
	log.Info("Starting render phase")
	objs := []*unstructured.Unstructured{}

	// Set configmap data
	data := configmap.Data
	cfg, err := ini.Load([]byte(data[operatortypes.ConfigMapDataKey]))
	if err != nil {
		return nil, errors.Wrap(err, "failed to load ConfigMap")
	}
	renderData := render.MakeRenderData()
	renderData.Data[operatortypes.NcpConfigMapRenderKey], err = generateConfigMap(cfg, operatortypes.NcpSections)
	if err != nil {
		return nil, errors.Wrap(err, "failed to render nsx-ncp ConfigMap")
	}
	renderData.Data[operatortypes.NodeAgentConfigMapRenderKey], err = generateConfigMap(cfg, operatortypes.AgentSections)
	if err != nil {
		return nil, errors.Wrap(err, "failed to render nsx-node-agent ConfigMap")
	}

	// Set NCP image
	ncpImage := os.Getenv(operatortypes.NcpImageEnv)
	if ncpImage == "" {
		ncpImage = "nsx-ncp:latest"
	}
	renderData.Data[operatortypes.NcpImageKey] = ncpImage

	// Set NCP replicas
	haEnabled := false
	if cfg.Section("ha").HasKey("enable") {
		haEnabled, err = strconv.ParseBool(cfg.Section("ha").Key("enable").Value())
		if err != nil {
			return nil, errors.Wrap(err, "failed to get ha option")
		}
	}
	if haEnabled {
		renderData.Data[operatortypes.NcpReplicasKey] = ncpReplicas
	} else {
		renderData.Data[operatortypes.NcpReplicasKey] = 1
		if ncpReplicas != 1 {
			log.Info(fmt.Sprintf("Set nsx-ncp deployment replicas to 1 instead of %d as HA is disabled",
				ncpReplicas))
		}
	}

	// Set LB secret
	if lbSecret != nil {
		renderData.Data[operatortypes.LbCertRenderKey] = base64.StdEncoding.EncodeToString(lbSecret.Data["tls.crt"])
		renderData.Data[operatortypes.LbKeyRenderKey] = base64.StdEncoding.EncodeToString(lbSecret.Data["tls.key"])
	} else {
		renderData.Data[operatortypes.LbCertRenderKey] = ""
		renderData.Data[operatortypes.LbKeyRenderKey] = ""
	}
	// Set NSX secret
	if nsxSecret != nil {
		renderData.Data[operatortypes.NsxCertRenderKey] = base64.StdEncoding.EncodeToString(nsxSecret.Data["tls.crt"])
		renderData.Data[operatortypes.NsxKeyRenderKey] = base64.StdEncoding.EncodeToString(nsxSecret.Data["tls.key"])
		renderData.Data[operatortypes.NsxCARenderKey] = base64.StdEncoding.EncodeToString(nsxSecret.Data["tls.ca"])
	} else {
		renderData.Data[operatortypes.NsxCertRenderKey] = ""
		renderData.Data[operatortypes.NsxKeyRenderKey] = ""
		renderData.Data[operatortypes.NsxCARenderKey] = ""
	}

	manifests, err := render.RenderDir(manifestDir, &renderData)
	if err != nil {
		return nil, errors.Wrap(err, "failed to render manifests")
	}

	objs = append(objs, manifests...)
	return objs, nil
}

func HasNetworkConfigChange(currConfig *configv1.Network, prevConfig *configv1.Network) bool {
	// only detect changes in spec.ClusterNetwork and spec.ServiceNetwork
	if !stringSliceEqual(currConfig.Spec.ServiceNetwork, prevConfig.Spec.ServiceNetwork) {
		// no point to check CIDRs as well
		return true
	}
	currCidrs := []string{}
	for _, cnet := range currConfig.Spec.ClusterNetwork {
		currCidrs = append(currCidrs, cnet.CIDR)
	}
	prevCidrs := []string{}
	for _, cnet := range prevConfig.Spec.ClusterNetwork {
		prevCidrs = append(prevCidrs, cnet.CIDR)
	}
	return stringSliceEqual(currCidrs, prevCidrs)
}

type podNeedChange struct {
	ncp       bool
	agent     bool
	bootstrap bool
}

func NeedApplyChange(currConfig *corev1.ConfigMap, prevConfig *corev1.ConfigMap) (
	podNeedChange, error) {
	if prevConfig == nil {
		return podNeedChange{true, true, true}, nil
	}
	currData := currConfig.Data
	prevData := prevConfig.Data
	// Compare the whole data
	if strings.Compare(strings.TrimSpace(currData[operatortypes.ConfigMapDataKey]),
		strings.TrimSpace(prevData[operatortypes.ConfigMapDataKey])) == 0 {
		return podNeedChange{false, false, false}, nil
	}
	// Compare every section to get different section slice
	currCfg, err := ini.Load([]byte(currData[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "Failed to load new ConfigMap")
		return podNeedChange{false, false, false}, err
	}
	prevCfg, err := ini.Load([]byte(prevData[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "Failed to load previous ConfigMap")
		return podNeedChange{false, false, false}, err
	}
	diffSecs := []string{}
	currSecs := currCfg.SectionStrings()
	for _, name := range currSecs {
		_, err = prevCfg.GetSection(name)
		if err != nil {
			if len(currCfg.Section(name).KeyStrings()) != 0 {
				diffSecs = append(diffSecs, name)
			}
			continue
		}
		if !reflect.DeepEqual(currCfg.Section(name).KeysHash(), prevCfg.Section(name).KeysHash()) {
			diffSecs = append(diffSecs, name)
		}
	}
	prevSecs := prevCfg.SectionStrings()
	for _, name := range prevSecs {
		_, err = currCfg.GetSection(name)
		if err != nil {
			if len(prevCfg.Section(name).KeyStrings()) != 0 {
				diffSecs = append(diffSecs, name)
			}
		}
	}
	// Check whether different sections impact on ncp, nsx-node-agent and nsx-ncp-bootstrap
	needChange := podNeedChange{}
	for _, sec := range diffSecs {
		if !needChange.ncp && inSlice(sec, operatortypes.NcpSections) {
			needChange.ncp = true
		}
		if !needChange.agent && inSlice(sec, operatortypes.AgentSections) {
			needChange.agent = true
		}
		bootstrapOpts, found := operatortypes.BootstrapOptions[sec]
		if !needChange.bootstrap && found {
			for _, opt := range bootstrapOpts {
				if currCfg.Section(sec).Key(opt).Value() != prevCfg.Section(sec).Key(opt).Value() {
					needChange.bootstrap = true
					break
				}
			}
		}
		if needChange.ncp && needChange.agent && needChange.bootstrap {
			break
		}
	}

	if len(diffSecs) > 0 {
		log.Info(fmt.Sprintf("Section %s changed", diffSecs))
	}

	return needChange, nil
}

func inSlice(str string, s []string) bool {
	for _, v := range s {
		if str == v {
			return true
		}
	}
	return false
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, v := range a {
		if !inSlice(v, b) {
			return false
		}
	}
	return true
}

func generateConfigMap(srcCfg *ini.File, sections []string) (string, error) {
	destCfg := ini.Empty()
	for _, name := range sections {
		srcSec, err := srcCfg.GetSection(name)
		if err != nil {
			continue
		}
		destCfg.NewSection(name)
		keys := srcSec.KeyStrings()
		for _, key := range keys {
			destCfg.Section(name).NewKey(key, srcSec.Key(key).Value())
		}
	}

	return iniWriteToString(destCfg)
}

func GenerateOperatorConfigMap(opConfigmap *corev1.ConfigMap, ncpConfigMap *corev1.ConfigMap,
	agentConfigMap *corev1.ConfigMap) error {
	ncpCfg, err := ini.Load([]byte(ncpConfigMap.Data[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "Failed to load nsx-ncp ConfigMap")
		return err
	}
	agentCfg, err := ini.Load([]byte(agentConfigMap.Data[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "Failed to load nsx-node-agent ConfigMap")
		return err
	}

	opCfg := ini.Empty()
	for _, name := range operatortypes.OperatorSections {
		sec, err := ncpCfg.GetSection(name)
		if err != nil {
			sec, err = agentCfg.GetSection(name)
			if err != nil {
				continue
			}
		}
		opCfg.NewSection(name)
		keys := sec.KeyStrings()
		for _, key := range keys {
			opCfg.Section(name).NewKey(key, sec.Key(key).Value())
		}
	}

	opConfigmap.Data[operatortypes.ConfigMapDataKey], err = iniWriteToString(opCfg)
	if err != nil {
		log.Error(err, "Failed to generate operator ConfigMap")
		return err
	}
	return nil
}

func iniWriteToString(cfg *ini.File) (string, error) {
	var buf bytes.Buffer
	_, err := cfg.WriteTo(&buf)
	if err != nil {
		return "", err
	}
	// go-ini does not write DEFAULT section name to buffer, so add it here
	cfgString := "\n[DEFAULT]\n" + buf.String()
	return cfgString, nil
}

func optionInConfigMap(configMap *corev1.ConfigMap, section string, key string) bool {
	cfg, err := ini.Load([]byte(configMap.Data[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "Failed to load ConfigMap")
		return false
	}
	sec, err := cfg.GetSection(section)
	if err != nil {
		return false
	}
	if sec.HasKey(key) && sec.Key(key).Value() != "" {
		return true
	}
	return false
}

func getOptionInConfigMap(configMap *corev1.ConfigMap, section string, key string) string {
	cfg, err := ini.Load([]byte(configMap.Data[operatortypes.ConfigMapDataKey]))
	if err != nil {
		log.Error(err, "Failed to load ConfigMap")
		return ""
	}
	sec, err := cfg.GetSection(section)
	if err != nil {
		log.Info(fmt.Sprintf("Section %s not found", section))
		return ""
	}
	if sec.HasKey(key) {
		return sec.Key(key).Value()
	}
	return ""
}

func IsMTUChanged(currConfigMap *corev1.ConfigMap, prevConfigMap *corev1.ConfigMap) bool {
	currMtu := getOptionInConfigMap(currConfigMap, "nsx_node_agent", "mtu")
	prevMtu := getOptionInConfigMap(prevConfigMap, "nsx_node_agent", "mtu")
	if currMtu == prevMtu {
		return false
	} else {
		return true
	}
}
