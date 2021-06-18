/*
Copyright 2020 The KubeSphere Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubevip

import (
	"fmt"
	kubekeyapiv1alpha1 "github.com/kubesphere/kubekey/apis/kubekey/v1alpha1"
	"github.com/kubesphere/kubekey/pkg/kubernetes/preinstall"
	"github.com/kubesphere/kubekey/pkg/util"
	"github.com/kubesphere/kubekey/pkg/util/manager"
	"github.com/lithammer/dedent"
	"github.com/pkg/errors"
	"io/ioutil"
	"os/exec"
	"strings"
	"text/template"
)

var kubevipTemlate = template.Must(template.New("kubevip").Parse(
	dedent.Dedent(`
---
apiVersion: v1
kind: Pod
metadata:
  creationTimestamp: null
  name: kubevip
  namespace: kube-system
spec:
  containers:
  - args:
    - start
    env:
    - name: vip_arp
      value: "true"
    - name: vip_interface
      value: {{ .Interface }}
    - name: vip_cidr
      value: "32"
    - name: cp_enable
      value: "true"
    - name: cp_namespace
      value: kube-system
    - name: vip_leaderelection
      value: "true"
    - name: vip_leaseduration
      value: "5"
    - name: vip_renewdeadline
      value: "3"
    - name: vip_retryperiod
      value: "1"
    - name: vip_address
      value: {{ .VIP }}
    image: {{ .KubevipImage }}
    imagePullPolicy: Always
    name: kubevip
    resources: {}
    securityContext:
      capabilities:
        add:
        - NET_ADMIN
        - NET_RAW
        - SYS_TIME
    volumeMounts:
    - mountPath: /etc/kubernetes/admin.conf
      name: kubeconfig
  hostNetwork: true
  volumes:
  - hostPath:
      path: /etc/kubernetes/admin.conf
    name: kubeconfig
status: {}
---
`)))

func GenerateKubevipManifest(mgr *manager.Manager, itfName string) (string, error) {
	return util.Render(kubevipTemlate, util.Data{
		"Interface":    itfName,
		"VIP":          mgr.Cluster.ControlPlaneEndpoint.Address,
		"KubevipImage": preinstall.GetImage(mgr, "kubevip").ImageName(),
	})
}

// InstallKubevip is used to install a load balancer for creating highly available clusters
func InstallKubevip(mgr *manager.Manager, node *kubekeyapiv1alpha1.HostCfg) error {
	itfName, err := GetInterfaceName(node)
	if err != nil {
		return errors.Wrap(errors.WithStack(err), fmt.Sprintf("Faield to get host [%s] interface: %s", node.Name, err))
	}

	fmt.Printf("[%s] generate kubevip manifest.\n", node.Name)
	kubevipStr, err := GenerateKubevipManifest(mgr, itfName)
	if err != nil {
		return errors.Wrap(errors.WithStack(err), fmt.Sprintf("Faield to generate kubevip manifest: %s", err))
	}

	if err := ioutil.WriteFile(fmt.Sprintf("%s/kubevip.yaml", mgr.WorkDir), []byte(kubevipStr), 0644); err != nil {
		return errors.Wrap(errors.WithStack(err), fmt.Sprintf("Failed to generate internal LB kubevip manifests: %s/kubevip.yaml", mgr.WorkDir))
	}

	kubevipBase64, err := exec.Command("/bin/bash", "-c", fmt.Sprintf("tar cfz - -C %s -T /dev/stdin <<< kubevip.yaml | base64 --wrap=0", mgr.WorkDir)).CombinedOutput()
	if err != nil {
		return errors.Wrap(errors.WithStack(err), "Failed to read internal LB kubevip manifests")
	}

	_, err = mgr.Runner.ExecuteCmd(fmt.Sprintf("sudo -E /bin/bash -c \"base64 -d <<< '%s' | tar xz -C %s\"", strings.TrimSpace(string(kubevipBase64)), "/etc/kubernetes/manifests"), 2, false)
	if err != nil {
		return errors.Wrap(errors.WithStack(err), "Failed to generate internal LB kubevip manifests")
	}

	return nil
}

func GetInterfaceName(node *kubekeyapiv1alpha1.HostCfg) (string, error) {
	output, err := exec.Command("/bin/sh", "-c", fmt.Sprintf("ip route | grep %s | awk -F '[ \\t*]' '{gsub(/\"/,\"\");for(i=0;i++<NF;)if($i==\"dev\")print $++i}'", node.InternalAddress)).CombinedOutput()
	if err != nil {
		fmt.Println(string(output))
		return "", err
	}

	outputArr := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(outputArr) >= 1 {
		return outputArr[0], nil
	} else {
		return "", errors.New(fmt.Sprintf("get cmd output err: %s", output))
	}
}

// CheckKubevip is used to check a internal load balancer. Check if the kubevip manifests exists in all master nodes.
func CheckKubevip(mgr *manager.Manager, node *kubekeyapiv1alpha1.HostCfg) error {
	if util.IsExist("/etc/kubernetes/manifests/kubevip.yaml") {
		fmt.Printf("[%s] kubevip manifest already exists.\n", node.Name)
	} else {
		fmt.Printf("[%s] kubevip manifest will be create.\n", node.Name)
		if err := InstallKubevip(mgr, node); err != nil {
			return err
		}
	}
	return nil
}
