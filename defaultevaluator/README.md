To build and deploy: (from this directory)
```
go test open-match.dev/open-match-ecosystem/... && \
\
export IMAGE_TAG=gcr.io/$(gcloud config list --format 'value(core.project)')/defaultevaluator:$(date +INDEV-%Y%m%d-%H%M%S) && \
\
docker build -f Dockerfile -t $IMAGE_TAG .. && \
docker push $IMAGE_TAG && \
\
kubectl apply -f defaultevaluator.yaml && \
kubectl set image --namespace open-match deployment/om-evaluator om-evaluator=$IMAGE_TAG

```
