/*
Copyright 2019 The Jetstack cert-manager contributors.

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	cmacme "github.com/jetstack/cert-manager/pkg/apis/acme/v1alpha2"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	"github.com/jetstack/cert-manager/pkg/logs"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

var ingressGVK = extv1beta1.SchemeGroupVersion.WithKind("Ingress")

func (c *controller) Sync(ctx context.Context, ing *extv1beta1.Ingress) error {
	log := logs.WithResource(logs.FromContext(ctx), ing)
	ctx = logs.NewContext(ctx, log)

	if !shouldSync(ing, c.defaults.autoCertificateAnnotations) {
		log.Info(fmt.Sprintf("not syncing ingress resource as it does not contain a %q or %q annotation",
			cmapi.IngressIssuerNameAnnotationKey, cmapi.IngressClusterIssuerNameAnnotationKey))
		return nil
	}

	issuerName, issuerKind, issuerGroup, err := c.issuerForIngress(ing)
	if err != nil {
		log.Error(err, "failed to determine issuer to be used for ingress resource")
		c.recorder.Eventf(ing, corev1.EventTypeWarning, "BadConfig", "Could not determine issuer for ingress due to bad annotations: %s",
			err)
		return nil
	}

	errs := c.validateIngress(ing)
	if len(errs) > 0 {
		errMsg := errs[0].Error()
		if len(errs) > 1 {
			errMsg = utilerrors.NewAggregate(errs).Error()
		}
		c.recorder.Eventf(ing, corev1.EventTypeWarning, "BadConfig", errMsg)
		return nil
	}

	newCrts, updateCrts, err := c.buildCertificates(ctx, ing, issuerName, issuerKind, issuerGroup)
	if err != nil {
		return err
	}

	for _, crt := range newCrts {
		_, err := c.cmClient.CertmanagerV1alpha2().Certificates(crt.Namespace).Create(context.TODO(), crt, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		c.recorder.Eventf(ing, corev1.EventTypeNormal, "CreateCertificate", "Successfully created Certificate %q", crt.Name)
	}

	for _, crt := range updateCrts {
		_, err := c.cmClient.CertmanagerV1alpha2().Certificates(crt.Namespace).Update(context.TODO(), crt, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		c.recorder.Eventf(ing, corev1.EventTypeNormal, "UpdateCertificate", "Successfully updated Certificate %q", crt.Name)
	}

	unrequiredCrts, err := c.findUnrequiredCertificates(ing)
	if err != nil {
		return err
	}

	for _, crt := range unrequiredCrts {
		err = c.cmClient.CertmanagerV1alpha2().Certificates(crt.Namespace).Delete(context.TODO(), crt.Name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		c.recorder.Eventf(ing, corev1.EventTypeNormal, "DeleteCertificate", "Successfully deleted unrequired Certificate %q", crt.Name)
	}

	return nil
}

func (c *controller) validateIngress(ing *extv1beta1.Ingress) []error {
	var errs []error
	namedSecrets := make(map[string]int)
	for i, tls := range ing.Spec.TLS {
		namedSecrets[tls.SecretName] += 1
		// validate the ingress TLS block
		if len(tls.Hosts) == 0 {
			errs = append(errs, fmt.Errorf("Secret %q for ingress TLS has no hosts specified", tls.SecretName))
		}
		if tls.SecretName == "" {
			errs = append(errs, fmt.Errorf("TLS entry %d for hosts %v must specify a secretName", i, tls.Hosts))
		}
	}
	for name, n := range namedSecrets {
		if n > 1 {
			errs = append(errs, fmt.Errorf("Duplicate TLS entry for secretName %q", name))
		}
	}
	return errs
}

func (c *controller) buildCertificates(ctx context.Context, ing *extv1beta1.Ingress,
	issuerName, issuerKind, issuerGroup string) (new, update []*cmapi.Certificate, _ error) {
	log := logs.FromContext(ctx)

	var newCrts []*cmapi.Certificate
	var updateCrts []*cmapi.Certificate
	for _, tls := range ing.Spec.TLS {
		existingCrt, err := c.certificateLister.Certificates(ing.Namespace).Get(tls.SecretName)
		if !apierrors.IsNotFound(err) && err != nil {
			return nil, nil, err
		}

		crt := &cmapi.Certificate{
			ObjectMeta: metav1.ObjectMeta{
				Name:            tls.SecretName,
				Namespace:       ing.Namespace,
				Labels:          ing.Labels,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ing, ingressGVK)},
			},
			Spec: cmapi.CertificateSpec{
				DNSNames:   tls.Hosts,
				SecretName: tls.SecretName,
				IssuerRef: cmmeta.ObjectReference{
					Name:  issuerName,
					Kind:  issuerKind,
					Group: issuerGroup,
				},
			},
		}

		err = c.setIssuerSpecificConfig(crt, ing, tls)
		if err != nil {
			return nil, nil, err
		}

		c.setCommonName(crt, ing, tls)

		// check if a Certificate for this TLS entry already exists, and if it
		// does then skip this entry
		if existingCrt != nil {
			log := logs.WithRelatedResource(log, existingCrt)
			log.Info("certificate already exists for ingress resource, ensuring it is up to date")

			if metav1.GetControllerOf(existingCrt) == nil {
				log.Info("certificate resource has no owner. refusing to update non-owned certificate resource for ingress")
				continue
			}

			if !metav1.IsControlledBy(existingCrt, ing) {
				log.Info("certificate resource is not owned by this ingress. refusing to update non-owned certificate resource for ingress")
				continue
			}

			if !certNeedsUpdate(existingCrt, crt) {
				log.Info("certificate resource is already up to date for ingress")
				continue
			}

			updateCrt := existingCrt.DeepCopy()

			updateCrt.Spec.DNSNames = tls.Hosts
			updateCrt.Spec.SecretName = tls.SecretName
			updateCrt.Spec.IssuerRef.Name = issuerName
			updateCrt.Spec.IssuerRef.Kind = issuerKind
			updateCrt.Spec.IssuerRef.Group = issuerGroup
			updateCrt.Spec.CommonName = ""
			updateCrt.Labels = ing.Labels
			err = c.setIssuerSpecificConfig(updateCrt, ing, tls)
			if err != nil {
				return nil, nil, err
			}
			updateCrts = append(updateCrts, updateCrt)
		} else {
			newCrts = append(newCrts, crt)
		}
	}
	return newCrts, updateCrts, nil
}

func (c *controller) findUnrequiredCertificates(ing *extv1beta1.Ingress) ([]*cmapi.Certificate, error) {
	var unrequired []*cmapi.Certificate
	// TODO: investigate selector which filters for certificates controlled by the ingress
	crts, err := c.certificateLister.Certificates(ing.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, crt := range crts {
		if isUnrequiredCertificate(crt, ing) {
			unrequired = append(unrequired, crt)
		}
	}

	return unrequired, nil
}

func isUnrequiredCertificate(crt *cmapi.Certificate, ing *extv1beta1.Ingress) bool {
	if !metav1.IsControlledBy(crt, ing) {
		return false
	}

	for _, tls := range ing.Spec.TLS {
		if crt.Spec.SecretName == tls.SecretName {
			return false
		}
	}
	return true
}

// certNeedsUpdate checks and returns true if two Certificates differ
func certNeedsUpdate(a, b *cmapi.Certificate) bool {
	if a.Name != b.Name {
		return true
	}

	// TODO: we may need to allow users to edit the managed Certificate resources
	// to add their own labels directly.
	// Right now, we'll reset/remove the label values back automatically.
	// Let's hope no other controllers do this automatically, else we'll start fighting...
	if !reflect.DeepEqual(a.Labels, b.Labels) {
		return true
	}

	if a.Spec.CommonName != b.Spec.CommonName {
		return true
	}

	if len(a.Spec.DNSNames) != len(b.Spec.DNSNames) {
		return true
	}

	for i := range a.Spec.DNSNames {
		if a.Spec.DNSNames[i] != b.Spec.DNSNames[i] {
			return true
		}
	}

	if a.Spec.SecretName != b.Spec.SecretName {
		return true
	}

	if a.Spec.IssuerRef.Name != b.Spec.IssuerRef.Name {
		return true
	}

	if a.Spec.IssuerRef.Kind != b.Spec.IssuerRef.Kind {
		return true
	}

	return false
}

func (c *controller) setIssuerSpecificConfig(crt *cmapi.Certificate, ing *extv1beta1.Ingress, tls extv1beta1.IngressTLS) error {
	ingAnnotations := ing.Annotations
	if ingAnnotations == nil {
		ingAnnotations = map[string]string{}
	}

	// for ACME issuers
	editInPlaceVal, _ := ingAnnotations[cmacme.IngressEditInPlaceAnnotationKey]
	editInPlace := editInPlaceVal == "true"
	if editInPlace {
		if crt.Annotations == nil {
			crt.Annotations = make(map[string]string)
		}
		crt.Annotations[cmacme.ACMECertificateHTTP01IngressNameOverride] = ing.Name
		// set IssueTemporaryCertificateAnnotation to true in order to behave
		// better when ingress-gce is being used.
		crt.Annotations[cmapi.IssueTemporaryCertificateAnnotation] = "true"
	}

	ingressClassVal, hasIngressClassVal := ingAnnotations[cmapi.IngressACMEIssuerHTTP01IngressClassAnnotationKey]
	if hasIngressClassVal {
		if crt.Annotations == nil {
			crt.Annotations = make(map[string]string)
		}
		crt.Annotations[cmacme.ACMECertificateHTTP01IngressClassOverride] = ingressClassVal
	}

	return nil
}

func (c *controller) setCommonName(crt *cmapi.Certificate, ing *extv1beta1.Ingress, tls extv1beta1.IngressTLS) {
	ingAnnotations := ing.Annotations
	if ingAnnotations == nil {
		ingAnnotations = map[string]string{}
	}

	// if annotation is set use that as CN
	if ingAnnotations[cmapi.CommonNameAnnotationKey] != "" {
		crt.Spec.CommonName = ingAnnotations[cmapi.CommonNameAnnotationKey]
		return
	}

	// if not set pick the first DNS name that is less than 64 characters
	// this is the length limit of the CN
	// if none if found we leave the CN empty
	for _, host := range tls.Hosts {
		if len(host) < 64 {
			crt.Spec.CommonName = host
			return
		}
	}

	return
}

// shouldSync returns true if this ingress should have a Certificate resource
// created for it
func shouldSync(ing *extv1beta1.Ingress, autoCertificateAnnotations []string) bool {
	annotations := ing.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	if _, ok := annotations[cmapi.IngressIssuerNameAnnotationKey]; ok {
		return true
	}
	if _, ok := annotations[cmapi.IngressClusterIssuerNameAnnotationKey]; ok {
		return true
	}
	for _, x := range autoCertificateAnnotations {
		if s, ok := annotations[x]; ok {
			if b, _ := strconv.ParseBool(s); b {
				return true
			}
		}
	}
	return false
}

// issuerForIngress will determine the issuer that should be specified on a
// Certificate created for the given Ingress resource. If one is not set, the
// default issuer given to the controller will be used.
func (c *controller) issuerForIngress(ing *extv1beta1.Ingress) (name, kind, group string, err error) {
	var errs []string

	name = c.defaults.issuerName
	kind = c.defaults.issuerKind
	group = c.defaults.issuerGroup
	annotations := ing.Annotations

	if annotations == nil {
		annotations = map[string]string{}
	}

	issuerName, issuerNameOK := annotations[cmapi.IngressIssuerNameAnnotationKey]
	if issuerNameOK {
		name = issuerName
		kind = cmapi.IssuerKind
	}

	clusterIssuerName, clusterIssuerNameOK := annotations[cmapi.IngressClusterIssuerNameAnnotationKey]
	if clusterIssuerNameOK {
		name = clusterIssuerName
		kind = cmapi.ClusterIssuerKind
	}

	kindName, kindNameOK := annotations[cmapi.IssuerKindAnnotationKey]
	if kindNameOK {
		kind = kindName
	}

	groupName, groupNameOK := annotations[cmapi.IssuerGroupAnnotationKey]
	if groupNameOK {
		group = groupName
	}

	if len(name) == 0 {
		errs = append(errs, "failed to determine issuer name to be used for ingress resource")
	}

	if issuerNameOK && clusterIssuerNameOK {
		errs = append(errs,
			fmt.Sprintf("both %q and %q may not be set",
				cmapi.IngressIssuerNameAnnotationKey, cmapi.IngressClusterIssuerNameAnnotationKey))
	}

	if clusterIssuerNameOK && groupNameOK {
		errs = append(errs,
			fmt.Sprintf("both %q and %q may not be set",
				cmapi.IngressClusterIssuerNameAnnotationKey, cmapi.IssuerGroupAnnotationKey))
	}

	if clusterIssuerNameOK && kindNameOK {
		errs = append(errs,
			fmt.Sprintf("both %q and %q may not be set",
				cmapi.IngressClusterIssuerNameAnnotationKey, cmapi.IssuerKindAnnotationKey))
	}

	if len(errs) > 0 {
		return "", "", "", errors.New(strings.Join(errs, ", "))
	}

	return name, kind, group, nil
}
