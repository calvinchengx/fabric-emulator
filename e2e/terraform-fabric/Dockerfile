# HashiCorp's published Terraform image, unmodified binary. The driver is a
# thin harness around `terraform init/apply/destroy`; nothing in Terraform
# or microsoft/fabric is patched. Pin a versioned tag — `latest` would make
# a Terraform release our flake.
FROM mirror.gcr.io/hashicorp/terraform:1.17.1@sha256:a5d5a5a892c98f60fcbfa81eac2a40fc19e7b3fa7719bccae10ff6111d22524a
RUN apk add --no-cache python3 openssl ca-certificates \
    && python3 --version
WORKDIR /work
COPY main.tf /work/main.tf
COPY driver.py /driver.py
ENTRYPOINT ["python3", "/driver.py"]
