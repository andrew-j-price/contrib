/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package abac

// Policy authorizes Kubernetes API actions using an Attribute-based access
// control scheme.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/apis/abac"
	"k8s.io/kubernetes/pkg/apis/abac/latest"
	"k8s.io/kubernetes/pkg/apis/abac/v0"
	_ "k8s.io/kubernetes/pkg/apis/abac/v1beta1"
	"k8s.io/kubernetes/pkg/auth/authorizer"
)

type policyLoadError struct {
	path string
	line int
	data []byte
	err  error
}

func (p policyLoadError) Error() string {
	if p.line >= 0 {
		return fmt.Sprintf("error reading policy file %s, line %d: %s: %v", p.path, p.line, string(p.data), p.err)
	}
	return fmt.Sprintf("error reading policy file %s: %v", p.path, p.err)
}

type policyList []*api.Policy

// TODO: Have policies be created via an API call and stored in REST storage.
func NewFromFile(path string) (policyList, error) {
	// File format is one map per line.  This allows easy concatentation of files,
	// comments in files, and identification of errors by line number.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	pl := make(policyList, 0)

	i := 0
	unversionedLines := 0
	for scanner.Scan() {
		i++
		p := &api.Policy{}
		b := scanner.Bytes()

		// skip comment lines and blank lines
		trimmed := strings.TrimSpace(string(b))
		if len(trimmed) == 0 || strings.HasPrefix(trimmed, "#") {
			continue
		}

		dataKind, err := api.Scheme.DataKind(b)
		if err != nil {
			return nil, policyLoadError{path, i, b, err}
		}

		if dataKind.IsEmpty() {
			unversionedLines++
			// Migrate unversioned policy object
			oldPolicy := &v0.Policy{}
			if err := latest.Codec.DecodeInto(b, oldPolicy); err != nil {
				return nil, policyLoadError{path, i, b, err}
			}
			if err := api.Scheme.Convert(oldPolicy, p); err != nil {
				return nil, policyLoadError{path, i, b, err}
			}
		} else {
			decodedObj, err := latest.Codec.Decode(b)
			if err != nil {
				return nil, policyLoadError{path, i, b, err}
			}
			decodedPolicy, ok := decodedObj.(*api.Policy)
			if !ok {
				return nil, policyLoadError{path, i, b, fmt.Errorf("unrecognized object: %#v", decodedObj)}
			}
			p = decodedPolicy
		}

		pl = append(pl, p)
	}

	if unversionedLines > 0 {
		glog.Warningf(`Policy file %s contained unversioned rules. See docs/admin/authorization.md#abac-mode for ABAC file format details.`, path)
	}

	if err := scanner.Err(); err != nil {
		return nil, policyLoadError{path, -1, nil, err}
	}
	return pl, nil
}

func matches(p api.Policy, a authorizer.Attributes) bool {
	if subjectMatches(p, a) {
		if verbMatches(p, a) {
			// Resource and non-resource requests are mutually exclusive, at most one will match a policy
			if resourceMatches(p, a) {
				return true
			}
			if nonResourceMatches(p, a) {
				return true
			}
		}
	}
	return false
}

// subjectMatches returns true if specified user and group properties in the policy match the attributes
func subjectMatches(p api.Policy, a authorizer.Attributes) bool {
	matched := false

	// If the policy specified a user, ensure it matches
	if len(p.Spec.User) > 0 {
		if p.Spec.User == "*" {
			matched = true
		} else {
			matched = p.Spec.User == a.GetUserName()
			if !matched {
				return false
			}
		}
	}

	// If the policy specified a group, ensure it matches
	if len(p.Spec.Group) > 0 {
		if p.Spec.Group == "*" {
			matched = true
		} else {
			matched = false
			for _, group := range a.GetGroups() {
				if p.Spec.Group == group {
					matched = true
				}
			}
			if !matched {
				return false
			}
		}
	}

	return matched
}

func verbMatches(p api.Policy, a authorizer.Attributes) bool {
	// TODO: match on verb

	// All policies allow read only requests
	if a.IsReadOnly() {
		return true
	}

	// Allow if policy is not readonly
	if !p.Spec.Readonly {
		return true
	}

	return false
}

func nonResourceMatches(p api.Policy, a authorizer.Attributes) bool {
	// A non-resource policy cannot match a resource request
	if !a.IsResourceRequest() {
		// Allow wildcard match
		if p.Spec.NonResourcePath == "*" {
			return true
		}
		// Allow exact match
		if p.Spec.NonResourcePath == a.GetPath() {
			return true
		}
		// Allow a trailing * subpath match
		if strings.HasSuffix(p.Spec.NonResourcePath, "*") && strings.HasPrefix(a.GetPath(), strings.TrimRight(p.Spec.NonResourcePath, "*")) {
			return true
		}
	}
	return false
}

func resourceMatches(p api.Policy, a authorizer.Attributes) bool {
	// A resource policy cannot match a non-resource request
	if a.IsResourceRequest() {
		if p.Spec.Namespace == "*" || p.Spec.Namespace == a.GetNamespace() {
			if p.Spec.Resource == "*" || p.Spec.Resource == a.GetResource() {
				if p.Spec.APIGroup == "*" || p.Spec.APIGroup == a.GetAPIGroup() {
					return true
				}
			}
		}
	}
	return false
}

// Authorizer implements authorizer.Authorize
func (pl policyList) Authorize(a authorizer.Attributes) error {
	for _, p := range pl {
		if matches(*p, a) {
			return nil
		}
	}
	return errors.New("No policy matched.")
	// TODO: Benchmark how much time policy matching takes with a medium size
	// policy file, compared to other steps such as encoding/decoding.
	// Then, add Caching only if needed.
}
