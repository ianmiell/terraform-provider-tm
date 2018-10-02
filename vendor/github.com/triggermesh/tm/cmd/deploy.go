package cmd

import (
	"fmt"
	"io/ioutil"
	"regexp"
	"time"

	buildv1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	servingv1alpha1 "github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	registry = "knative-local-registry:5000"
)

var (
	Image, Source, URL, PullPolicy,
	Path, Revision, CPU, Memory, Storage string
	Env, Labels []string
	df          = "/workspace/Dockerfile"
)

// deployCmd represents the deploy command
var deployCmd = &cobra.Command{
	Use:     "deploy",
	Short:   "Deploy knative service",
	Args:    cobra.ExactArgs(1),
	Example: "tm -n default deploy foo --from-image gcr.io/google-samples/hello-app:1.0",
	Run: func(cmd *cobra.Command, args []string) {
		if err := DeployService(args); err != nil {
			log.Errorln(err)
		}
	},
}

func init() {
	deployCmd.Flags().StringVar(&Image, "from-image", "", "Image to deploy")
	deployCmd.Flags().StringVar(&Source, "from-source", "", "Git source URL to deploy")
	deployCmd.Flags().StringVar(&Revision, "revision", "master", "May be used with \"--from-source\" flag: git revision (branch, tag, commit SHA or ref) to clone")
	deployCmd.Flags().StringVar(&Path, "from-file", "", "Local file path to deploy")
	deployCmd.Flags().StringVar(&URL, "from-url", "", "File source URL to deploy")
	// deployCmd.Flags().StringVar(&cpu, "cpu", "", "Limit number of core units for service")
	// deployCmd.Flags().StringVar(&memory, "memory", "", "Limit amount of memory for service, eg. 100M, 1.5G")
	// deployCmd.Flags().StringVar(&storage, "storage", "", "Limit volume size for service root device, eg. 200M, 5G")
	// deployCmd.Flags().Int32Var(&port, "port", 8080, "Custom service port")
	deployCmd.Flags().StringVar(&PullPolicy, "image-pull-policy", "Always", "Image pull policy")
	// deployCmd.Flags().StringSliceVar(&secrets, "secrets", []string{}, "Name of Secrets to mount into service environment")
	deployCmd.Flags().StringSliceVarP(&Labels, "label", "l", []string{}, "Service labels")
	deployCmd.Flags().StringSliceVarP(&Env, "env", "e", []string{}, "Environment variables of the service, eg. `--env foo=bar`")
	rootCmd.AddCommand(deployCmd)
}

func DeployService(args []string) error {
	configuration := servingv1alpha1.ConfigurationSpec{}
	switch {
	case len(Image) != 0:
		configuration = fromImage(args)
	case len(Source) != 0:
		if err := createConfigMap(nil); err != nil {
			return err
		}
		if err := kanikoBuildTemplate(); err != nil {
			return err
		}
		configuration = fromSource(args)
	case len(URL) != 0:
		if err := getterBuildTemplate(); err != nil {
			return err
		}
		configuration = fromURL(args)
	case len(Path) != 0:
		filebody, err := ioutil.ReadFile(Path)
		if err != nil {
			return err
		}
		data := make(map[string]string)
		data[args[0]] = string(filebody)
		if err := createConfigMap(data); err != nil {
			return err
		}
		if err := kanikoBuildTemplate(); err != nil {
			return err
		}
		configuration = fromFile(args)
	}

	// res, err := resources()
	// if err != nil {
	// 	return err
	// }

	// configuration.RevisionTemplate.Spec.Container.Ports = []corev1.ContainerPort{corev1.ContainerPort{ContainerPort: port}}
	// configuration.RevisionTemplate.Spec.Container.Resources.Requests = res
	configuration.RevisionTemplate.Spec.Container.ImagePullPolicy = corev1.PullPolicy(PullPolicy)
	configuration.RevisionTemplate.Spec.Container.Env = append(getEnv(Env), corev1.EnvVar{
		Name:  "timestamp",
		Value: time.Now().Format("2006-01-02 15:04:05")})

	s := servingv1alpha1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "serving.knative.dev/servingv1alpha1",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name:      args[0],
			Namespace: Namespace,
			CreationTimestamp: metav1.Time{
				time.Now(),
			},
			Labels: getLabels(Labels),
		},

		Spec: servingv1alpha1.ServiceSpec{
			RunLatest: &servingv1alpha1.RunLatestType{
				Configuration: configuration,
			},
		},
	}

	log.Debugf("Service object: %+v\n", s)
	log.Debugf("Service specs: %+v\n", s.Spec.RunLatest)

	service, err := serving.ServingV1alpha1().Services(Namespace).Get(args[0], metav1.GetOptions{})
	if err == nil {
		s.ObjectMeta.ResourceVersion = service.ObjectMeta.ResourceVersion
		service, err = serving.ServingV1alpha1().Services(Namespace).Update(&s)
		if err != nil {
			return err
		}
		log.Infof("Service update started. Run \"tm -n %s get revisions\" to see available revisions\n", Namespace)
	} else if k8sErrors.IsNotFound(err) {
		service, err := serving.ServingV1alpha1().Services(Namespace).Create(&s)
		if err != nil {
			return err
		}
		log.Infof("Deployment started. Run \"tm -n %s describe service %s\" to see the details\n", Namespace, service.Name)
	} else {
		return err
	}
	return nil
}

func kanikoBuildTemplate() error {
	_, err := build.BuildV1alpha1().BuildTemplates(Namespace).Get("kaniko", metav1.GetOptions{})
	if err == nil {
		log.Debugln("kaniko already exist")
	} else if k8sErrors.IsNotFound(err) {
		log.Debugln("deploying kaniko")

		bt := buildv1alpha1.BuildTemplate{
			TypeMeta: metav1.TypeMeta{
				Kind:       "BuildTemplate",
				APIVersion: "build.knative.dev/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kaniko",
				Namespace: Namespace,
			},
			Spec: buildv1alpha1.BuildTemplateSpec{
				Parameters: []buildv1alpha1.ParameterSpec{
					{
						Name: "IMAGE",
					},
					{
						Name:    "DOCKERFILE",
						Default: &df,
					},
				},
				Steps: []corev1.Container{
					{
						Name:  "build-and-push",
						Image: "gcr.io/kaniko-project/executor",
						Args:  []string{"--dockerfile=${DOCKERFILE}", "--destination=${IMAGE}", "--skip-tls-verify"},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "docker-file",
								MountPath: "/docker-file",
							},
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "docker-file",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "dockerfile"},
							},
						},
					},
				},
			},
		}
		_, err = build.BuildV1alpha1().BuildTemplates(Namespace).Create(&bt)
		return err
	}
	return err
}

func getterBuildTemplate() error {
	_, err := build.BuildV1alpha1().BuildTemplates(Namespace).Get("getandbuild", metav1.GetOptions{})
	if err == nil {
		log.Debugln("getandbuild template already exist")
	} else if k8sErrors.IsNotFound(err) {
		log.Debugln("deploying getandbuild template")
		bt := buildv1alpha1.BuildTemplate{
			TypeMeta: metav1.TypeMeta{
				Kind:       "BuildTemplate",
				APIVersion: "build.knative.dev/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "getandbuild",
				Namespace: Namespace,
			},
			Spec: buildv1alpha1.BuildTemplateSpec{
				Parameters: []buildv1alpha1.ParameterSpec{
					{
						Name: "URL",
					},
					{
						Name: "IMAGE",
					},
					{
						Name:    "DOCKERFILE",
						Default: &df,
					},
				},
				Steps: []corev1.Container{
					{
						Name:  "get",
						Image: "index.docker.io/byrnedo/alpine-curl",
						Args:  []string{"-o", "${DOCKERFILE}", "${URL}"},
					},
					{
						Name:  "build-and-push",
						Image: "gcr.io/kaniko-project/executor",
						Args:  []string{"--dockerfile=${DOCKERFILE}", "--destination=${IMAGE}", "--skip-tls-verify"},
						Env: []corev1.EnvVar{
							{
								Name:  "DOCKER_CONFIG",
								Value: "/docker-config",
							},
						},
					},
				},
			},
		}
		_, err = build.BuildV1alpha1().BuildTemplates(Namespace).Create(&bt)
		return err
	}
	return err
}

func fromImage(args []string) servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		RevisionTemplate: servingv1alpha1.RevisionTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"sidecar.istio.io/inject": "true",
				},
				Name: args[0],
			},
			Spec: servingv1alpha1.RevisionSpec{
				Container: corev1.Container{
					Image: Image,
				},
			},
		},
	}
}

func fromSource(args []string) servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		Build: &buildv1alpha1.BuildSpec{
			Source: &buildv1alpha1.SourceSpec{
				Git: &buildv1alpha1.GitSourceSpec{
					Url:      Source,
					Revision: Revision,
				},
			},
			Template: &buildv1alpha1.TemplateInstantiationSpec{
				Name: "kaniko",
				Arguments: []buildv1alpha1.ArgumentSpec{
					{
						Name:  "IMAGE",
						Value: fmt.Sprintf("%s/%s-%s-source:latest", registry, Namespace, args[0]),
					},
				},
			},
		},
		RevisionTemplate: servingv1alpha1.RevisionTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"sidecar.istio.io/inject": "true",
				},
				Name: args[0],
			},
			Spec: servingv1alpha1.RevisionSpec{
				Container: corev1.Container{
					Image: fmt.Sprintf("%s/%s-%s-source:latest", registry, Namespace, args[0]),
				},
			},
		},
	}
}

func fromURL(args []string) servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		Build: &buildv1alpha1.BuildSpec{
			Source: &buildv1alpha1.SourceSpec{
				Custom: &corev1.Container{
					Image: "registry.hub.docker.com/library/busybox",
				},
			},
			Template: &buildv1alpha1.TemplateInstantiationSpec{
				Name: "getandbuild",
				Arguments: []buildv1alpha1.ArgumentSpec{
					{
						Name:  "IMAGE",
						Value: fmt.Sprintf("%s/%s-%s-url:latest", registry, Namespace, args[0]),
					},
					{
						Name:  "URL",
						Value: URL,
					},
				},
			},
		},
		RevisionTemplate: servingv1alpha1.RevisionTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"sidecar.istio.io/inject": "true",
				},
				Name: args[0],
			},
			Spec: servingv1alpha1.RevisionSpec{
				Container: corev1.Container{
					Image: fmt.Sprintf("%s/%s-%s-url:latest", registry, Namespace, args[0]),
				},
			},
		},
	}
}

func fromFile(args []string) servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		Build: &buildv1alpha1.BuildSpec{
			Source: &buildv1alpha1.SourceSpec{
				Custom: &corev1.Container{
					Image: "registry.hub.docker.com/library/busybox",
				},
			},
			Template: &buildv1alpha1.TemplateInstantiationSpec{
				Name: "kaniko",
				Arguments: []buildv1alpha1.ArgumentSpec{
					{
						Name:  "IMAGE",
						Value: fmt.Sprintf("%s/%s-%s-file:latest", registry, Namespace, args[0]),
					},
					{
						Name:  "DOCKERFILE",
						Value: "/docker-file/" + args[0],
					},
				},
			},
		},
		RevisionTemplate: servingv1alpha1.RevisionTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: args[0],
				Annotations: map[string]string{
					"sidecar.istio.io/inject": "true",
				},
			},
			Spec: servingv1alpha1.RevisionSpec{
				Container: corev1.Container{
					Image: fmt.Sprintf("%s/%s-%s-file:latest", registry, Namespace, args[0]),
				},
			},
		},
	}
}

func createConfigMap(data map[string]string) error {
	newmap := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dockerfile",
			Namespace: Namespace,
		},
		Data: data,
	}
	cm, err := core.CoreV1().ConfigMaps(Namespace).Get("dockerfile", metav1.GetOptions{})
	if err == nil {
		newmap.ObjectMeta.ResourceVersion = cm.ObjectMeta.ResourceVersion
		_, err = core.CoreV1().ConfigMaps(Namespace).Update(&newmap)
		return err
	} else if k8sErrors.IsNotFound(err) {
		_, err = core.CoreV1().ConfigMaps(Namespace).Create(&newmap)
		return err
	}
	return err
}

func getLabels(slice []string) map[string]string {
	m := make(map[string]string)
	m["created-by"] = "tm"
	for _, s := range slice {
		t := regexp.MustCompile("[:=]").Split(s, 2)
		if len(t) != 2 {
			log.Warnf("Can't parse label argument %s", s)
			continue
		}
		m[t[0]] = t[1]
	}
	return m
}

func getEnv(slice []string) []corev1.EnvVar {
	m := []corev1.EnvVar{}
	for _, s := range slice {
		t := regexp.MustCompile("[:=]").Split(s, 2)
		if len(t) != 2 {
			log.Warnf("Can't parse environment argument %s", s)
			continue
		}
		m = append(m, corev1.EnvVar{Name: t[0], Value: t[1]})
	}
	return m
}

func resources() (map[corev1.ResourceName]resource.Quantity, error) {
	res := make(map[corev1.ResourceName]resource.Quantity)
	if cores, err := resource.ParseQuantity(CPU); CPU != "" && err != nil {
		return nil, err
	} else {
		res[corev1.ResourceCPU] = cores
	}
	if ram, err := resource.ParseQuantity(Memory); Memory != "" && err != nil {
		return nil, err
	} else {
		res[corev1.ResourceMemory] = ram
	}
	if disk, err := resource.ParseQuantity(Storage); Storage != "" && err != nil {
		return nil, err
	} else {
		res[corev1.ResourceStorage] = disk
	}
	return res, nil
}
