package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	"log/slog"
	"time"

	bizstorage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	datastorage "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/storage"
	servicestorage "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/storage"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

const storageDownloadTTL = 10 * time.Minute

type storageComponents struct {
	service *servicestorage.Service
	worker  *server.StorageDeleteWorker
}

// openStorage 装配 Storage 用例。Kubernetes 客户端复用现有 kubeconfig 配置；
// 对象存储未配置 endpoint 时只关闭对象浏览，不影响 PVC 管理。
func openStorage(_ context.Context, bc *conf.Bootstrap, instances *instanceComponents) (*storageComponents, error) {
	kube, err := clientcmd.BuildConfigFromFlags("", bc.GetNetwork().GetKubeconfig())
	if err != nil {
		return nil, fmt.Errorf("storage Kubernetes configuration invalid")
	}
	kube.QPS, kube.Burst = 5, 10
	kube.Timeout = 15 * time.Second
	client, err := dynamic.NewForConfig(kube)
	if err != nil {
		return nil, err
	}
	volumes, err := datastorage.NewKubernetes(client)
	if err != nil {
		return nil, err
	}
	var objects bizstorage.ObjectPort
	if object := bc.GetObjectStorage(); object != nil && object.GetEndpoint() != "" {
		s3, err := datastorage.NewS3(datastorage.S3Config{
			Endpoint:        object.GetEndpoint(),
			BrowserEndpoint: object.GetBrowserEndpoint(),
			Region:          object.GetRegion(),
			PathStyle:       object.GetPathStyle(),
			CredentialsFile: object.GetCredentialsFile(),
			CAFile:          object.GetCaFile(),
		})
		if err != nil {
			return nil, err
		}
		if s3 != nil {
			objects = s3
		}
	}
	var guard bizstorage.BorrowedPVCGuard
	var observeDeleteWrite func(string)
	if instances != nil {
		guard = instances.repo
		observeDeleteWrite = instances.repo.ObservePVCDeleteWrite
	}
	useCase, err := bizstorage.NewUseCase(volumes, objects, bizstorage.Config{
		ClusterID: bc.GetNetwork().GetClusterId(), DeleteExecutor: uuid.NewString(), BorrowedPVCGuard: guard,
		ObserveDeleteWrite: observeDeleteWrite,
		NamespacePrefix:    bc.GetNetwork().GetNamespacePrefix(),
		Buckets:            bc.GetObjectStorage().Buckets(),
		DownloadTTL:        storageDownloadTTL,
	})
	if err != nil {
		return nil, err
	}
	handler, err := servicestorage.NewService(useCase)
	if err != nil {
		return nil, err
	}
	out := &storageComponents{service: handler}
	if instances != nil {
		out.worker = server.NewStorageDeleteWorker(useCase, instances.repo.CheckReady, slog.Default())
	}
	return out, nil
}
