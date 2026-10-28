# HashiCorp's published Terraform image, unmodified binary. The driver is a
# thin harness around `terraform init/apply/destroy`; nothing in Terraform
# or microsoft/fabric is patched. Pin a versioned tag — `latest` would make
# a Terraform release our flake.
FROM mirror.gcr.io/hashicorp/terraform:1.17.0@sha256:4d1b496a256f2989bccdb04b9104a4ede97234b10fba90259e173e905ec4fb58
RUN apk add --no-cache python3 openssl ca-certificates \
    && python3 --version
WORKDIR /work
COPY main.tf /work/main.tf
COPY driver.py /driver.py
ENTRYPOINT ["python3", "/driver.py"]
