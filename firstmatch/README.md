To deploy:

1. Deploy and configure core open-match:
```
kubectl apply -f https://open-match.dev/install/v0.8.0/yaml/01-open-match-core.yaml -f coreconfig.yaml --namespace=open-match

```

2. Follow deployment steps in defaultevaluator/README.md.

3. Build and deploy demo: (from this directory)
```
go test open-match.dev/open-match-ecosystem/... && \
\
export IMAGE_NAME=gcr.io/$(gcloud config list --format 'value(core.project)')/firstmatch:$(date +INDEV-%Y%m%d-%H%M%S) && \
\
docker build -f Dockerfile -t $IMAGE_NAME .. && \
docker push $IMAGE_NAME && \
\
kubectl apply -f firstmatch.yaml && \
kubectl set image --namespace open-match-firstmatch deployment/om-demo om-demo-firstmatch=$IMAGE_NAME && \
\
sleep 5 && \
kubectl port-forward --namespace open-match-firstmatch service/om-demo 51507:51507

```
