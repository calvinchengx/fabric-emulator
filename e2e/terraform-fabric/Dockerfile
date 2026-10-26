# HashiCorp's published Terraform image, unmodified binary. The driver is a
# thin harness around `terraform init/apply/destroy`; nothing in Terraform
# or microsoft/fabric is patched. Pin a versioned tag — `latest` would make
# a Terraform release our flake.
FROM mirror.gcr.io/hashicorp/terraform:1.12.2@sha256:f5ac787eee9d292b6a3b97d40f04019ce08189d356233fc73d5ec7ef8529cce2
RUN apk add --no-cache python3 openssl ca-certificates \
    && python3 --version
WORKDIR /work
COPY main.tf /work/main.tf
COPY driver.py /driver.py
ENTRYPOINT ["python3", "/driver.py"]
